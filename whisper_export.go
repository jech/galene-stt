package main

import (
	"C"
	"fmt"
)

//export whisper_segment_callback
func whisper_segment_callback(s *C.char) {
	ss := C.GoString(s)
	if displayAsCaption || displayAsChat {
		kind := ""
		if displayAsCaption {
			kind = "caption"
		}
		writer.write(&clientMessage{
			Type:     "chat",
			Kind:     kind,
			Source:   myId,
			Username: &username,
			Value:    ss,
		})
	} else {
		fmt.Println(ss)
	}
}
