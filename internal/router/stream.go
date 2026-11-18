package router

import (
	"bytes"
	"encoding/json"
)

// observer never changes the forwarded stream. Its own memory is bounded.
type observer struct {
	line        []byte
	data        []byte
	dropping    bool
	Usage       Usage
	Started     bool
	Delta       bool
	Stopped     bool
	Invalid     bool
	StreamError bool
	Model       string
}

func (o *observer) Feed(p []byte) {
	for _, c := range p {
		if c == '\n' {
			if !o.dropping {
				o.parseLine(bytes.TrimSuffix(o.line, []byte{'\r'}))
			}
			o.line = o.line[:0]
			o.dropping = false
			continue
		}
		if !o.dropping {
			if len(o.line) >= 2<<20 {
				o.Invalid = true
				o.dropping = true
				o.line = o.line[:0]
			} else {
				o.line = append(o.line, c)
			}
		}
	}
}
func (o *observer) parseLine(line []byte) {
	if len(line) == 0 {
		if len(o.data) > 0 {
			o.event(o.data)
			o.data = o.data[:0]
		}
		return
	}
	if bytes.HasPrefix(line, []byte("data:")) {
		value := bytes.TrimPrefix(line, []byte("data:"))
		value = bytes.TrimPrefix(value, []byte(" "))
		if len(o.data)+len(value) > 2<<20 {
			o.Invalid = true
			o.data = o.data[:0]
			return
		}
		if len(o.data) > 0 {
			o.data = append(o.data, '\n')
		}
		o.data = append(o.data, value...)
	}
}
func (o *observer) event(b []byte) {
	var ev struct {
		Type    string          `json:"type"`
		Usage   json.RawMessage `json:"usage"`
		Message struct {
			Usage json.RawMessage `json:"usage"`
			Model string          `json:"model"`
		} `json:"message"`
	}
	if json.Unmarshal(b, &ev) != nil {
		o.Invalid = true
		return
	}
	switch ev.Type {
	case "message_start":
		o.Started = true
		o.Model = ev.Message.Model
		if o.Usage.Merge(ev.Message.Usage) != nil {
			o.Invalid = true
		}
	case "message_delta":
		o.Delta = true
		if o.Usage.Merge(ev.Usage) != nil {
			o.Invalid = true
		}
	case "message_stop":
		o.Stopped = true
	case "error":
		o.StreamError = true
	}
}
func (o *observer) Complete() bool {
	return o.Started && o.Delta && o.Stopped && !o.Invalid && !o.StreamError
}
