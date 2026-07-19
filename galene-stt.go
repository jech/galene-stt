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
	"path/filepath"
	"slices"
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

var modelDirectory string
var modelArchitecture string
var galeneClient *gclient.Client
var username string

// The model's native sample rate
const sampleRate = 16000

func main() {
	var password string
	var insecure bool
	var dumpaudio string

	cacheDir, err := os.UserCacheDir()
	if err != nil {
		log.Fatalf("UserCacheDir: %v", err)
	}

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
	flag.StringVar(&modelDirectory, "model",
		filepath.Join(cacheDir,
			"moonshine_voice/download.moonshine.ai/model/medium-streaming-en/quantized",
		),
		"model `directory`")
	flag.StringVar(&modelArchitecture, "model-arch", "medium-streaming",
		"model `architecture`")
	flag.StringVar(&username, "username", "speech-to-text",
		"`username` to use for login")
	flag.StringVar(&password, "password", "",
		"`password` to use for login")
	flag.BoolVar(&insecure, "insecure", false,
		"don't check server certificates")
	flag.BoolVar(&debug, "debug", false,
		"enable protocol logging")
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

	transcriber, err := CreateTranscriber(modelDirectory, modelArchitecture)
	if err != nil {
		log.Fatalf("CreateTranscriber: %v", err)
	}
	defer DestroyTranscriber(transcriber)

	client := gclient.NewClient()

	var ir interceptor.Registry
	var me webrtc.MediaEngine
	err = webrtc.RegisterDefaultInterceptors(&me, &ir)
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
	if err != nil {
		log.Fatalf("Join: %v", err)
	}

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
				case "fail":
					log.Printf("Couldn't join: %v", e.Value)
					break outer
				case "join", "change":
					client.Request(
						map[string][]string{
							"": {"audio"},
						},
					)
				}
			case gclient.DownTrackEvent:
				gotTrack(e.Track, e.Receiver, transcriber)
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

func gotTrack(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, transcriber Transcriber) {
	codec := track.Codec()
	if !strings.EqualFold(codec.MimeType, "audio/opus") {
		log.Printf("Unexpected track type %v", codec.MimeType)
		return
	}

	go func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, transcriber Transcriber) {
		err := rtpLoop(track, receiver, transcriber)
		if err != nil {
			log.Printf("RTP loop: %v", err)
		}
	}(track, receiver, transcriber)
}

func dumpAudio(pcm []float32) error {
	if dumpAudioFile != nil {
		err := dumpAudioFile.Write(pcm)
		return err
	}
	return nil
}

func display(s string) error {
	if s == "" {
		return nil
	}

	if displayAsCaption {
		return galeneClient.Chat("caption", "", s)
	}
	if displayAsChat {
		return galeneClient.Chat("", "", s)
	}
	_, err := fmt.Println(s)
	return err
}


var ErrBacklogged = errors.New("backlogged, dropping audio")

func rtpLoop(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver, transcriber Transcriber) error {
	decoder, err := opus.NewDecoder(sampleRate, 1)
	if err != nil {
		return err
	}
	defer decoder.Destroy()

	stream, err := CreateStream(transcriber)
	if err != nil {
		return err
	}
	err = StartStream(transcriber, stream)
	if err != nil {
		DestroyStream(transcriber, stream)
		return err
	}
	// packets are usually 20ms, so this allows up to 2s latency
	// when under load
	workerCh := make(chan []float32, 100)
	defer close(workerCh)

	go func(workerCh <-chan []float32) {
		lastTranscribe := time.Now()
		for {
			m, ok := <-workerCh
			if !ok {
				break
			}
			err = AddAudio(transcriber, stream, m, sampleRate)
			if err != nil {
				log.Printf("AddAudio: %v", err)
			}
			if time.Since(lastTranscribe) > 200*time.Millisecond {
				lastTranscribe = time.Now()
				s, err := Transcribe(transcriber, stream)
				if err != nil {
					log.Printf("Transcribe: %v", err)
					continue
				}
				display(s)
			}
		}
		s, err := StopStream(transcriber, stream)
		if err != nil {
			log.Printf("StopStream: %v", err)
		} else {
			display(s)
		}
		DestroyStream(transcriber, stream)
	}(workerCh)

	buf := make([]byte, 2048)
	out := make([]float32, 8192)
	var buffered *rtp.Packet
	var lastSeqno uint16
	var nextTS uint32

	var packet rtp.Packet

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

	transcribe := func(data []float32) error {
		dumpAudio(data)
		select {
		case workerCh <- slices.Clone(data):
			return nil
		default:
			return ErrBacklogged
		}
	}

	decode := func(p *rtp.Packet) error {
		n, err := decoder.DecodeFloat(p.Payload, out, false)
		if err == nil {
			err = transcribe(out[:n])
		}
		lastSeqno = p.SequenceNumber
		nextTS = p.Timestamp + uint32(3*n)
		return err
	}

	decodeFEC := func(p *rtp.Packet, samples int) error {
		n, err := decoder.DecodeFloat(p.Payload, out[:samples], true)
		if err == nil {
			err = transcribe(out[:n])
		}
		lastSeqno = p.SequenceNumber - 1
		nextTS = p.Timestamp
		return err
	}

	for {
		bytes, _, err := track.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
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
			}
		}

		err = decode(next)
		if err != nil {
			log.Printf("Decode: %v", err)
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
	}

	return nil
}
