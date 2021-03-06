// Copyright (c) 2017 Shivaram Lingamneni <slingamn@cs.stanford.edu>
// released under the MIT license

package irc

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goshuirc/irc-go/ircfmt"
	"github.com/oragono/oragono/irc/caps"
)

const (
	// RegisterTimeout is how long clients have to register before we disconnect them
	RegisterTimeout = time.Minute
	// DefaultIdleTimeout is how long without traffic before we send the client a PING
	DefaultIdleTimeout = time.Minute + 30*time.Second
	// For Tor clients, we send a PING at least every 30 seconds, as a workaround for this bug
	// (single-onion circuits will close unless the client sends data once every 60 seconds):
	// https://bugs.torproject.org/29665
	TorIdleTimeout = time.Second * 30
	// This is how long a client gets without sending any message, including the PONG to our
	// PING, before we disconnect them:
	DefaultTotalTimeout = 2*time.Minute + 30*time.Second
	// Resumeable clients (clients who have negotiated caps.Resume) get longer:
	ResumeableTotalTimeout = 3*time.Minute + 30*time.Second
)

// client idleness state machine

type TimerState uint

const (
	TimerUnregistered TimerState = iota // client is unregistered
	TimerActive                         // client is actively sending commands
	TimerIdle                           // client is idle, we sent PING and are waiting for PONG
	TimerDead                           // client was terminated
)

type IdleTimer struct {
	sync.Mutex // tier 1

	// immutable after construction
	registerTimeout time.Duration
	client          *Client

	// mutable
	idleTimeout time.Duration
	quitTimeout time.Duration
	state       TimerState
	timer       *time.Timer
}

// Initialize sets up an IdleTimer and starts counting idle time;
// if there is no activity from the client, it will eventually be stopped.
func (it *IdleTimer) Initialize(client *Client) {
	it.client = client
	it.registerTimeout = RegisterTimeout
	it.idleTimeout, it.quitTimeout = it.recomputeDurations()

	it.Lock()
	defer it.Unlock()
	it.state = TimerUnregistered
	it.resetTimeout()
}

// recomputeDurations recomputes the idle and quit durations, given the client's caps.
func (it *IdleTimer) recomputeDurations() (idleTimeout, quitTimeout time.Duration) {
	totalTimeout := DefaultTotalTimeout
	// if they have the resume cap, wait longer before pinging them out
	// to give them a chance to resume their connection
	if it.client.capabilities.Has(caps.Resume) {
		totalTimeout = ResumeableTotalTimeout
	}

	idleTimeout = DefaultIdleTimeout
	if it.client.isTor {
		idleTimeout = TorIdleTimeout
	}

	quitTimeout = totalTimeout - idleTimeout
	return
}

func (it *IdleTimer) Touch() {
	idleTimeout, quitTimeout := it.recomputeDurations()

	it.Lock()
	defer it.Unlock()
	it.idleTimeout, it.quitTimeout = idleTimeout, quitTimeout
	// a touch transitions TimerUnregistered or TimerIdle into TimerActive
	if it.state != TimerDead {
		it.state = TimerActive
		it.resetTimeout()
	}
}

func (it *IdleTimer) processTimeout() {
	idleTimeout, quitTimeout := it.recomputeDurations()

	var previousState TimerState
	func() {
		it.Lock()
		defer it.Unlock()
		it.idleTimeout, it.quitTimeout = idleTimeout, quitTimeout
		previousState = it.state
		// TimerActive transitions to TimerIdle, all others to TimerDead
		if it.state == TimerActive {
			// send them a ping, give them time to respond
			it.state = TimerIdle
			it.resetTimeout()
		} else {
			it.state = TimerDead
		}
	}()

	if previousState == TimerActive {
		it.client.Ping()
	} else {
		it.client.Quit(it.quitMessage(previousState))
		it.client.destroy(false)
	}
}

// Stop stops counting idle time.
func (it *IdleTimer) Stop() {
	if it == nil {
		return
	}

	it.Lock()
	defer it.Unlock()
	it.state = TimerDead
	it.resetTimeout()
}

func (it *IdleTimer) resetTimeout() {
	if it.timer != nil {
		it.timer.Stop()
	}
	var nextTimeout time.Duration
	switch it.state {
	case TimerUnregistered:
		nextTimeout = it.registerTimeout
	case TimerActive:
		nextTimeout = it.idleTimeout
	case TimerIdle:
		nextTimeout = it.quitTimeout
	case TimerDead:
		return
	}
	it.timer = time.AfterFunc(nextTimeout, it.processTimeout)
}

func (it *IdleTimer) quitMessage(state TimerState) string {
	switch state {
	case TimerUnregistered:
		return fmt.Sprintf("Registration timeout: %v", it.registerTimeout)
	case TimerIdle:
		// how many seconds before registered clients are timed out (IdleTimeout plus QuitTimeout).
		it.Lock()
		defer it.Unlock()
		return fmt.Sprintf("Ping timeout: %v", (it.idleTimeout + it.quitTimeout))
	default:
		// shouldn't happen
		return ""
	}
}

// NickTimer manages timing out of clients who are squatting reserved nicks
type NickTimer struct {
	sync.Mutex // tier 1

	// immutable after construction
	client *Client

	// mutable
	nick           string
	accountForNick string
	account        string
	timeout        time.Duration
	timer          *time.Timer
	enabled        uint32
}

// Initialize sets up a NickTimer, based on server config settings.
func (nt *NickTimer) Initialize(client *Client) {
	if nt.client == nil {
		nt.client = client // placate the race detector
	}

	config := &client.server.Config().Accounts.NickReservation
	enabled := config.Enabled && (config.Method == NickReservationWithTimeout || config.AllowCustomEnforcement)

	nt.Lock()
	defer nt.Unlock()
	nt.timeout = config.RenameTimeout
	if enabled {
		atomic.StoreUint32(&nt.enabled, 1)
	} else {
		nt.stopInternal()
	}
}

func (nt *NickTimer) Enabled() bool {
	return atomic.LoadUint32(&nt.enabled) == 1
}

func (nt *NickTimer) Timeout() (timeout time.Duration) {
	nt.Lock()
	timeout = nt.timeout
	nt.Unlock()
	return
}

// Touch records a nick change and updates the timer as necessary
func (nt *NickTimer) Touch() {
	if !nt.Enabled() {
		return
	}

	cfnick, skeleton := nt.client.uniqueIdentifiers()
	account := nt.client.Account()
	accountForNick, method := nt.client.server.accounts.EnforcementStatus(cfnick, skeleton)
	enforceTimeout := method == NickReservationWithTimeout

	var shouldWarn, shouldRename bool

	func() {
		nt.Lock()
		defer nt.Unlock()

		// the timer will not reset as long as the squatter is targeting the same account
		accountChanged := accountForNick != nt.accountForNick
		// change state
		nt.nick = cfnick
		nt.account = account
		nt.accountForNick = accountForNick
		delinquent := accountForNick != "" && accountForNick != account

		if nt.timer != nil && (!enforceTimeout || !delinquent || accountChanged) {
			nt.timer.Stop()
			nt.timer = nil
		}
		if enforceTimeout && delinquent && (accountChanged || nt.timer == nil) {
			nt.timer = time.AfterFunc(nt.timeout, nt.processTimeout)
			shouldWarn = true
		} else if method == NickReservationStrict && delinquent {
			shouldRename = true // this can happen if reservation was enabled by rehash
		}
	}()

	if shouldWarn {
		nt.client.Send(nil, "NickServ", "NOTICE", nt.client.Nick(), fmt.Sprintf(ircfmt.Unescape(nt.client.t(nsTimeoutNotice)), nt.Timeout()))
	} else if shouldRename {
		nt.client.Notice(nt.client.t("Nickname is reserved by a different account"))
		nt.client.server.RandomlyRename(nt.client)
	}
}

// Stop stops counting time and cleans up the timer
func (nt *NickTimer) Stop() {
	nt.Lock()
	defer nt.Unlock()
	nt.stopInternal()
}

func (nt *NickTimer) stopInternal() {
	if nt.timer != nil {
		nt.timer.Stop()
		nt.timer = nil
	}
	atomic.StoreUint32(&nt.enabled, 0)
}

func (nt *NickTimer) processTimeout() {
	baseMsg := "Nick is reserved and authentication timeout expired: %v"
	nt.client.Notice(fmt.Sprintf(nt.client.t(baseMsg), nt.Timeout()))
	nt.client.server.RandomlyRename(nt.client)
}
