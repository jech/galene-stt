package main

import (
	"C"
)

//export whisper_segment_callback
func whisper_segment_callback(s *C.char) {
	writer.write(&clientMessage{
		Type: "chat",
		Source: myId,
		Username: &username,
		Value: C.GoString(s),
	})
}
