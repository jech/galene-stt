package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jech/galene-stt/opus"
	"github.com/jech/galene-stt/wav"

	"github.com/gorilla/websocket"
	"github.com/jech/gclient"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

var httpClient http.Client
var debug bool
var displayAsCaption, displayAsChat bool

var dumpAudioFile *wav.Writer

type workMessage struct {
	data []float32
}

var worker *messageWriter[workMessage]
var modelFilename string
var galeneClient *gclient.Client
var username string

func main() {
	var password string
	var insecure bool
	var silenceTime, silence float64
	var language string
	var translate, useGPU bool
	var dumpaudio string

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr,
			"Usage: %s group [file...]\n", os.Args[0],
		)
		flag.PrintDefaults()
	}
	flag.BoolVar(&displayAsCaption, "caption", false,
		"display inferred text as captions",
	)
	flag.BoolVar(&displayAsChat, "chat", false,
		"display inferred text as chat messages",
	)
	flag.StringVar(&modelFilename, "model", "models/ggml-medium.bin",
		"whisper model `filename`")
	flag.StringVar(&username, "username", "speech-to-text",
		"`username` to use for login")
	flag.StringVar(&password, "password", "",
		"`password` to use for login")
	flag.BoolVar(&insecure, "insecure", false,
		"don't check server certificates")
	flag.BoolVar(&debug, "debug", false,
		"enable protocol logging")
	flag.Float64Var(&silenceTime, "silence-time", 0.3,
		"`seconds` of silence required to start a new phrase")
	flag.Float64Var(&silence, "silence", 0.07,
		"maximum `volume` required to start a new phrase")
	flag.BoolVar(&keepSilence, "keep-silence", false,
		"don't discard segments of silence, pass them to the engine")
	flag.StringVar(&language, "lang", "en",
		"`language` of input, or \"auto\" for autodetection")
	flag.BoolVar(&translate, "translate", false,
		"translate foreign languages")
	flag.BoolVar(&useGPU, "gpu", true, "run on GPU if possible")
	flag.StringVar(&dumpaudio, "dumpaudio", "",
		"dump decoded audio to `filename`")
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}

	if debug {
		gclient.Debug = true
	}

	if dumpaudio != "" {
		var err error
		dumpAudioFile, err = wav.Create(dumpaudio)
		if err != nil {
			log.Fatalf("Create %v: %v", dumpaudio, err)
		}
		defer dumpAudioFile.Close()
	}

	silenceSamples = int(silenceTime * 16000)
	silenceSquared = float32(silence * silence)

	client := gclient.NewClient()

	var ir interceptor.Registry
	var me webrtc.MediaEngine
	err := webrtc.RegisterDefaultInterceptors(&me, &ir)
	if err != nil {
		log.Fatalf("RegisterDefaultInterceptors: %v", err)
	}
	err = me.RegisterCodec(
		webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				webrtc.MimeTypeOpus, 48000, 2,
				"minptime=10;useinbandfec=1", nil,
			},
			PayloadType: 111,
		}, webrtc.RTPCodecTypeAudio,
	)
	if err != nil {
		log.Fatalf("RegisterCodec: %v", err)
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(&me),
		webrtc.WithInterceptorRegistry(&ir),
	)

	client.SetAPI(api)

	if insecure {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		client.SetHTTPClient(&http.Client{
			Transport: t,
		})

		d := *websocket.DefaultDialer
		d.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		client.SetDialer(&d)
	}

	err = client.Connect(context.Background(), flag.Arg(0))
	if err != nil {
		log.Fatalf("Connect: %v", err)
	}
	galeneClient = client

	err = client.Join(
		context.Background(), flag.Arg(0), username, password,
	)

	worker = newWriter[workMessage](2)
	defer close(worker.ch)
	go func(worker *messageWriter[workMessage]) {
		defer close(worker.done)

		wContext, err := whisperInit(modelFilename, useGPU)
		if err != nil {
			log.Printf("Whisper: %v", err)
			return
		}
		defer whisperClose(wContext)

		for {
			work, ok := <-worker.ch
			if !ok {
				return
			}
			for len(work.data) < minSamples {
				work.data = append(work.data, 0.0)
			}
			err := whisper(wContext, work.data, language, translate)
			if err != nil {
				log.Printf("Whisper: %v", err)
				return
			}
		}
	}(worker)

	terminate := make(chan os.Signal, 1)
	signal.Notify(terminate, syscall.SIGINT, syscall.SIGTERM)

outer:
	for {
		select {
		case <-terminate:
			break outer
		case e := <-client.EventCh:
			switch e := e.(type) {
			case gclient.JoinedEvent:
				switch e.Kind {
				case "failed":
					log.Printf("Couldn't join: %v", e.Value)
					break outer
				case "join", "change":
					client.Request(
						map[string][]string{
							"": []string{"audio"},
						},
					)
				}
			case gclient.DownTrackEvent:
				gotTrack(e.Track, e.Receiver)
			case gclient.UserMessageEvent:
				if e.Kind == "error" || e.Kind == "warning" {
					log.Printf(
						"The server said: %v: %v",
						e.Kind, e.Value,
					)
					break
				}
				log.Printf("Unexpected usermessage of kind %v",
					e.Kind)
			case error:
				log.Printf("Protocol error: %v", e)
				break outer
			}
		}
	}
	client.Close()
}

func debugf(fmt string, args ...interface{}) {
	if debug {
		log.Printf(fmt, args...)
	}
}

type messageWriter[T any] struct {
	ch   chan T
	done chan struct{}
}

func newWriter[T any](capacity int) *messageWriter[T] {
	return &messageWriter[T]{
		ch:   make(chan T, capacity),
		done: make(chan struct{}),
	}
}

func (writer *messageWriter[T]) write(m T) error {
	select {
	case writer.ch <- m:
		return nil
	case <-writer.done:
		return io.EOF
	}
}

func gotTrack(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	codec := track.Codec()
	if !strings.EqualFold(codec.MimeType, "audio/opus") {
		log.Printf("Unexpected track type %v", codec.MimeType)
		return
	}

	go func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		err := rtpLoop(track, receiver)
		if err != nil {
			log.Printf("RTP loop: %v", err)
		}
	}(track, receiver)
}

func dumpAudio(pcm []float32) error {
	if dumpAudioFile != nil {
		err := dumpAudioFile.Write(pcm)
		return err
	}
	return nil
}

const overlapSamples = 200 * 16
const minSamples = 17600
const maxSamples = 3 * 16000

var silenceSamples int
var silenceSquared float32
var keepSilence bool

const silenceSamplingInterval = 16000 / 200

func rtpLoop(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) error {
	decoder, err := opus.NewDecoder(16000, 1)
	if err != nil {
		return err
	}
	defer decoder.Destroy()

	buf := make([]byte, 2048)
	var buffered *rtp.Packet
	out := make([]float32, 0, 2*maxSamples)
	var lastSeqno uint16
	var nextTS uint32

	var packet rtp.Packet

	silence := 0
	checkSilence := func(data []float32) {
		// chop the data into silenceSamplingInterval chunks
		i := 0
		for i < len(data) {
			count := silenceSamplingInterval
			if count < len(data)-i {
				count = len(data) - i
			}
			var s float32
			// compute the average volume of each chunk
			for j := i; j < i+count; j++ {
				v := data[j]
				s += v * v
			}
			// if the chunk was below the threshold,
			// accumulate silence
			if s <= silenceSquared*float32(count) {
				silence += count
			} else {
				silence = 0
			}
			i += count
		}
	}

	flush := func(all bool) error {
		if len(out) <= overlapSamples {
			if all {
				out = out[:0]
			}
			return nil
		}

		m := workMessage{
			data: out,
		}
		select {
		case worker.ch <- m:
			if all {
				out = out[:0]
			} else {
				copy(out, out[len(out)-overlapSamples:])
				out = out[:overlapSamples]
			}
		case <-worker.done:
			return errors.New("whisper failure")
		default:
			log.Printf("Backlogged, dropping %vs of audio",
				float32(len(out))/16000,
			)
			out = out[:0]
		}
		return nil
	}

	go func(receiver *webrtc.RTPReceiver) {
		buf := make([]byte, 2048)
		for {
			_, _, err := receiver.Read(buf)
			if err != nil {
				if err == io.EOF {
					return
				}
				log.Printf("Read RTCP: %v", err)
				time.Sleep(time.Second)
				continue
			}
		}
	}(receiver)

	decode := func(p *rtp.Packet) error {
		n, err := decoder.DecodeFloat(
			p.Payload, out[len(out):cap(out)], false,
		)
		if err != nil {
			return err
		}
		dumpAudio(out[len(out) : len(out)+n])
		checkSilence(out[len(out) : len(out)+n])
		out = out[:len(out)+n]
		lastSeqno = p.SequenceNumber
		nextTS = p.Timestamp + uint32(3*n)
		return nil
	}

	decodeFEC := func(p *rtp.Packet, samples int) error {
		if cap(out)-len(out) < samples {
			return errors.New("buffer overflow")
		}
		n, err := decoder.DecodeFloat(
			p.Payload, out[len(out):len(out)+samples], true,
		)
		if err != nil {
			return err
		}
		dumpAudio(out[len(out) : len(out)+n])
		checkSilence(out[len(out) : len(out)+n])
		out = out[:len(out)+n]
		lastSeqno = p.SequenceNumber
		nextTS = p.Timestamp + uint32(3*n)
		return nil
	}

	for {
		bytes, _, err := track.Read(buf)
		if err != nil {
			err2 := flush(true)
			if err != io.EOF {
				return err
			}
			return err2
		}
		err = packet.Unmarshal(buf[:bytes])
		if err != nil {
			log.Printf("%v", err)
			continue
		}

		var next *rtp.Packet
		fec := false

		if len(out) == 0 {
			next = &packet
		} else {
			delta := packet.SequenceNumber - lastSeqno
			if delta == 0 || delta >= 0xFF00 {
				// late packet, drop it
				continue
			}
			if delta == 1 {
				// in-order packet
				next = &packet
			} else if buffered == nil {
				// one out-of-order packet
				buffered = packet.Clone()
				continue
			} else if delta == 2 {
				// two out-of-order packets, apply FEC
				fec = true
				next = &packet
			} else {
				bdelta := buffered.SequenceNumber - lastSeqno
				if bdelta == 2 {
					// apply FEC to the buffered packet
					fec = true
					next = buffered
					buffered = packet.Clone()
				} else {
					debugf("Packet drop, "+
						"delta=%v, bdelta=%v",
						delta, bdelta)
					err := flush(true)
					if err != nil {
						return err
					}
					if delta == bdelta {
						buffered = nil
						next = &packet
					} else if delta < bdelta {
						next = &packet
					} else {
						next = buffered
						buffered = packet.Clone()
					}
				}
			}
		}

		if fec {
			err = decodeFEC(next, int(next.Timestamp-nextTS)/3)
			if err != nil {
				log.Printf("Decode FEC: %v", err)
				err := flush(true)
				if err != nil {
					return err
				}
			}
		}

		err = decode(next)
		if err != nil {
			log.Printf("Decode: %v", err)
			silence = 0
			err := flush(true)
			if err != nil {
				return err
			}
			continue
		}

		if buffered != nil &&
			buffered.Timestamp == nextTS &&
			buffered.SequenceNumber == lastSeqno+1 {
			err := decode(buffered)
			if err != nil {
				log.Printf("Decode buffered: %v", err)
			}
			buffered = nil
		}

		if !keepSilence &&
			len(out) >= silenceSamples && silence >= len(out) {
			debugf("Discarding %v of silence",
				time.Duration(len(out))*time.Second/16000,
			)
			out = out[:0]
			continue
		}

		if len(out) >= minSamples && silence >= silenceSamples {
			err := flush(true)
			if err != nil {
				return err
			}
		}

		if len(out) >= maxSamples {
			err := flush(false)
			if err != nil {
				return err
			}
		}
	}
}
