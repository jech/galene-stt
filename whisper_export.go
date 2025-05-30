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
		galeneClient.Chat(kind, username, ss)
	} else {
		fmt.Println(ss)
	}
}
