// Copyright (c) 2017 Daniel Oaks <daniel@danieloaks.net>
// released under the MIT license

package utils

import "bytes"

// WordWrap wraps the given text into a series of lines that don't exceed lineWidth characters.
func WordWrap(text string, lineWidth int) []string {
	var lines []string
	var cacheLine, cacheWord bytes.Buffer

	for _, char := range text {
		if char == '\r' {
			continue
		} else if char == '\n' {
			cacheLine.Write(cacheWord.Bytes())
			lines = append(lines, cacheLine.String())
			cacheWord.Reset()
			cacheLine.Reset()
		} else if (char == ' ' || char == '-') && cacheLine.Len()+cacheWord.Len()+1 < lineWidth {
			// natural word boundary
			cacheLine.Write(cacheWord.Bytes())
			cacheLine.WriteRune(char)
			cacheWord.Reset()
		} else if lineWidth <= cacheLine.Len()+cacheWord.Len()+1 {
			// time to wrap to next line
			if cacheLine.Len() < (lineWidth / 2) {
				// this word takes up more than half a line... just split in the middle of the word
				cacheLine.Write(cacheWord.Bytes())
				cacheLine.WriteRune(char)
				cacheWord.Reset()
			} else {
				cacheWord.WriteRune(char)
			}
			lines = append(lines, cacheLine.String())
			cacheLine.Reset()
		} else {
			// normal character
			cacheWord.WriteRune(char)
		}
	}
	if 0 < cacheWord.Len() {
		cacheLine.Write(cacheWord.Bytes())
	}
	if 0 < cacheLine.Len() {
		lines = append(lines, cacheLine.String())
	}

	return lines
}

type MessagePair struct {
	Message string
	Msgid   string
}

// SplitMessage represents a message that's been split for sending.
type SplitMessage struct {
	MessagePair
	Wrapped []MessagePair // if this is nil, `Message` didn't need wrapping and can be sent to anyone
}

const defaultLineWidth = 400

func MakeSplitMessage(original string, origIs512 bool) (result SplitMessage) {
	result.Message = original
	result.Msgid = GenerateSecretToken()

	if !origIs512 && defaultLineWidth < len(original) {
		wrapped := WordWrap(original, defaultLineWidth)
		result.Wrapped = make([]MessagePair, len(wrapped))
		for i, wrappedMessage := range wrapped {
			result.Wrapped[i] = MessagePair{
				Message: wrappedMessage,
				Msgid:   GenerateSecretToken(),
			}
		}
	}

	return
}
