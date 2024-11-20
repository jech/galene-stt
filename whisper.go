package main

import (
	"errors"
	"fmt"
	"log"
	"time"
	"unsafe"
)

/*
#cgo LDFLAGS: -lwhisper

#include <stdlib.h>
#include <stdio.h>
#include <string.h>
#include "whisper.h"

struct whisper_context *w_init(char *model_filename, int gpu) {
      struct whisper_context_params cparams = whisper_context_default_params();
      cparams.use_gpu = gpu ? true : false;
      struct whisper_context *ctx = whisper_init_from_file_with_params(
          model_filename, cparams
      );
      return ctx;
}

extern void whisper_segment_callback(const char*);

static void new_segment_callback(struct whisper_context *ctx, struct whisper_state *state, int n_new, void * user_data) {
    int n = whisper_full_n_segments(ctx);
    for(int i = n - n_new; i < n; i++) {
        const char *s = whisper_full_get_segment_text(ctx, i);
        if(strcmp(s, " [BLANK_AUDIO]") == 0)
            continue;
        whisper_segment_callback(s);
    }
}

int whisper(struct whisper_context *ctx, void *data, int size,
            char *language, int translate) {
     struct whisper_full_params params =
         whisper_full_default_params(WHISPER_SAMPLING_GREEDY);
     params.language = language;
     if(strcmp(language, "auto") == 0) {
         params.detect_language = 1;
     }
     params.translate = translate;
     params.new_segment_callback = new_segment_callback;
     return whisper_full(ctx, params, data, size);
}
*/
import "C"

type whisperContext *C.struct_whisper_context

func whisperInit(modelFilename string, gpu bool) (whisperContext, error) {
	f := C.CString(modelFilename)
	defer C.free(unsafe.Pointer(f))
	g := C.int(0)
	if gpu {
		g = 1
	}
	c := C.w_init(f, g)
	if c == nil {
		return nil, errors.New("failed to initialise Whisper context")
	}
	return whisperContext(c), nil
}

func whisper(ctx whisperContext, buf []float32, language string, translate bool) error {
	l := C.CString(language)
	defer C.free(unsafe.Pointer(l))
	t := C.int(0)
	if translate {
		t = 1
	}
	begin := time.Now()
	rc := C.whisper(ctx, unsafe.Pointer(&buf[0]), C.int(len(buf)), l, t)
	end := time.Now()

	if rc != 0 {
		return fmt.Errorf("whisper returned %v", rc)
	}

	if debug {
		log.Printf("Processed %v of audio in %v",
			time.Duration(len(buf))*time.Second/16000,
			end.Sub(begin),
		)
	}
	return nil
}

func whisperClose(ctx whisperContext) {
	C.whisper_free(ctx)
}
