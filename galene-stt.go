package main

import (
	"bytes"
	crand "crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/jech/galene-stt/opus"

	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type groupStatus struct {
	Name        string `json:"name"`
	Redirect    string `json:"redirect,omitempty"`
	Location    string `json:"location,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	AuthServer  string `json:"authServer,omitempty"`
	AuthPortal  string `json:"authPortal,omitempty"`
	Locked      bool   `json:"locked,omitempty"`
	ClientCount *int   `json:"clientCount,omitempty"`
}

type clientMessage struct {
	Type             string                   `json:"type"`
	Version          []string                 `json:"version,omitempty"`
	Kind             string                   `json:"kind,omitempty"`
	Error            string                   `json:"error,omitempty"`
	Id               string                   `json:"id,omitempty"`
	Replace          string                   `json:"replace,omitempty"`
	Source           string                   `json:"source,omitempty"`
	Dest             string                   `json:"dest,omitempty"`
	Username         *string                  `json:"username,omitempty"`
	Password         string                   `json:"password,omitempty"`
	Token            string                   `json:"token,omitempty"`
	Privileged       bool                     `json:"privileged,omitempty"`
	Permissions      []string                 `json:"permissions,omitempty"`
	Status           *groupStatus             `json:"status,omitempty"`
	Data             map[string]any           `json:"data,omitempty"`
	Group            string                   `json:"group,omitempty"`
	Value            any                      `json:"value,omitempty"`
	NoEcho           bool                     `json:"noecho,omitempty"`
	Time             string                   `json:"time,omitempty"`
	SDP              string                   `json:"sdp,omitempty"`
	Candidate        *webrtc.ICECandidateInit `json:"candidate,omitempty"`
	Label            string                   `json:"label,omitempty"`
	Request          any                      `json:"request,omitempty"`
	RTCConfiguration *webrtc.Configuration    `json:"rtcConfiguration,omitempty"`
}

var myId, username string
var client http.Client
var rtcConfiguration *webrtc.Configuration
var debug bool
var displayAsCaption, displayAsChat bool

var dumpAudioFile *os.File

type connection struct {
	id string
	pc *webrtc.PeerConnection
}

var connections = make(map[string]*connection)
var writer *messageWriter[*clientMessage]

type workMessage struct {
	data []float32
}

var worker *messageWriter[workMessage]
var api *webrtc.API
var modelFilename string

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

	if dumpaudio != "" {
		var err error
		dumpAudioFile, err = os.Create(dumpaudio)
		if err != nil {
			log.Fatalf("Create %v: %v", dumpaudio, err)
		}
		defer dumpAudioFile.Close()
	}

	silenceSamples = int(silenceTime * 16000)
	silenceSquared = float32(silence * silence)

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

	api = webrtc.NewAPI(
		webrtc.WithMediaEngine(&me),
		webrtc.WithInterceptorRegistry(&ir),
	)

	dialer := websocket.DefaultDialer
	if insecure {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		client.Transport = t

		d := *dialer
		d.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		dialer = &d
	}

	group, err := url.Parse(flag.Arg(0))
	if err != nil {
		log.Fatalf("Parse group: %v", err)
	}
	token := group.Query().Get("token")
	group.RawQuery = ""

	status, err := getGroupStatus(group.String())
	if err != nil {
		log.Fatalf("Get group status: %v", err)
	}

	if token == "" && status.AuthServer != "" {
		var err error
		token, err = getToken(
			status.AuthServer, group.String(), username, password,
		)
		if err != nil {
			log.Fatalf("Get token: %v", err)
		}
	}

	if status.Endpoint == "" {
		log.Fatalf("Server didn't provide endpoint.")
	}

	ws, _, err := dialer.Dial(status.Endpoint, nil)
	if err != nil {
		log.Fatalf("Connect to server: %v", err)
	}
	defer ws.Close()
	writer = newWriter[*clientMessage](8)
	go writerLoop(ws, writer)

	readerCh := make(chan *clientMessage, 1)
	go readerLoop(ws, readerCh)

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

	myId = makeId()

	writer.write(&clientMessage{
		Type:    "handshake",
		Version: []string{"2", "1"},
		Id:      myId,
	})

	m := <-readerCh
	if m == nil {
		log.Fatal("Connection closed")
		return
	}
	if m.Type != "handshake" {
		log.Fatalf("Unexpected message %v", m.Type)
	}

	m = &clientMessage{
		Type:     "join",
		Kind:     "join",
		Group:    status.Name,
		Username: &username,
	}
	if token != "" {
		m.Token = token
	} else if password != "" {
		// don't leak passwords if we obtained a token
		m.Password = password
	}
	writer.write(m)

	done := make(chan struct{})

	terminate := make(chan os.Signal, 1)
	signal.Notify(terminate, syscall.SIGINT, syscall.SIGTERM)

outer:
	for {
		select {
		case <-terminate:
			break outer
		case <-done:
			break outer
		case <-worker.done:
			log.Println("Whisper failure")
			break outer
		case m = <-readerCh:
			if m == nil {
				log.Println("Connection closed")
				break outer
			}
		}

		switch m.Type {
		case "ping":
			writer.write(&clientMessage{
				Type: "pong",
			})
		case "joined":
			switch m.Kind {
			case "fail":
				log.Printf("Couldn't join: %v", m.Value)
				break outer
			case "join", "change":
				rtcConfiguration = m.RTCConfiguration
				writer.write(&clientMessage{
					Type: "request",
					Request: map[string][]string{
						"": []string{"audio"},
					},
				})
			case "leave":
				rtcConfiguration = nil
				break outer
			}
		case "offer":
			username := ""
			if m.Username != nil {
				username = *m.Username
			}
			err := gotOffer(m.Id, m.Label,
				m.Source, username,
				m.SDP, m.Replace)
			if err != nil {
				log.Printf("gotOffer: %v", err)
				writer.write(&clientMessage{
					Type: "abort",
					Id:   m.Id,
				})
			}
		case "ice":
			err := gotRemoteIce(m.Id, m.Candidate)
			if err != nil {
				log.Printf("Remote ICE: %v", err)
			}
		case "usermessage":
			if m.Kind == "error" || m.Kind == "warning" {
				log.Printf(
					"The server said: %v: %v",
					m.Kind, m.Value,
				)
				break
			}
			log.Printf("Unexpected usermessage of kind %v", m.Kind)
		case "close":
			gotClose(m.Id)
		}
	}

	close(writer.ch)
	<-writer.done
}

func debugf(fmt string, args ...interface{}) {
	if debug {
		log.Printf(fmt, args...)
	}
}

func makeId() string {
	rawId := make([]byte, 8)
	crand.Read(rawId)
	return base64.RawURLEncoding.EncodeToString(rawId)
}

func readerLoop(ws *websocket.Conn, ch chan<- *clientMessage) {
	defer close(ch)
	for {
		var m clientMessage
		err := ws.ReadJSON(&m)
		if err != nil {
			debugf("ReadJSON: %v", err)
			return
		}
		if debug {
			j, _ := json.Marshal(m)
			debugf("<- %v", string(j))
		}
		ch <- &m
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

func writerLoop(ws *websocket.Conn, writer *messageWriter[*clientMessage]) {
	defer close(writer.done)
	for {
		m, ok := <-writer.ch
		if !ok {
			break
		}
		if debug {
			j, _ := json.Marshal(m)
			debugf("-> %v", string(j))
		}
		err := ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err != nil {
			debugf("Writer deadline: %v", err)
			return
		}
		err = ws.WriteJSON(m)
		if err != nil {
			debugf("WriteJSON: %v", err)
			return
		}
	}
	ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(100*time.Millisecond),
	)
}

func getGroupStatus(group string) (groupStatus, error) {
	s, err := url.Parse(group)
	if err != nil {
		return groupStatus{}, err
	}
	s.Path = path.Join(s.Path, ".status.json")
	s.RawPath = ""
	resp, err := client.Get(s.String())
	if err != nil {
		return groupStatus{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return groupStatus{}, errors.New(resp.Status)
	}

	decoder := json.NewDecoder(resp.Body)
	var status groupStatus
	err = decoder.Decode(&status)
	if err != nil {
		return groupStatus{}, err
	}
	return status, nil
}

func getToken(server, group, username, password string) (string, error) {
	request := map[string]any{
		"username": username,
		"location": group,
		"password": password,
	}
	req, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	resp, err := client.Post(
		server, "application/json", bytes.NewReader(req),
	)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return "", nil
	}

	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}

	t, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(t), nil
}

func (conn *connection) close() error {
	delete(connections, conn.id)
	return conn.pc.Close()
}

func gotOffer(id, label, source, username, offer, replace string) error {
	if replace != "" {
		otherconn := connections[replace]
		if otherconn != nil {
			otherconn.close()
		}
	}

	conn := connections[id]
	if conn == nil {
		if rtcConfiguration == nil {
			return errors.New("no configuration")
		}
		pc, err := api.NewPeerConnection(*rtcConfiguration)
		if err != nil {
			return err
		}
		_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio)
		if err != nil {
			pc.Close()
			return err
		}
		pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
			if candidate == nil {
				return
			}
			init := candidate.ToJSON()
			writer.write(&clientMessage{
				Type:      "ice",
				Id:        id,
				Candidate: &init,
			})
		})
		pc.OnTrack(func(t *webrtc.TrackRemote, r *webrtc.RTPReceiver) {
			gotTrack(t, r)
		})
		conn = &connection{
			id: id,
			pc: pc,
		}
		connections[id] = conn
	}

	err := conn.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offer,
	})
	if err != nil {
		conn.close()
		return err
	}

	answer, err := conn.pc.CreateAnswer(nil)
	if err != nil {
		conn.close()
		return err
	}

	err = conn.pc.SetLocalDescription(answer)
	if err != nil {
		conn.close()
		return err
	}

	writer.write(&clientMessage{
		Type: "answer",
		Id:   id,
		SDP:  conn.pc.LocalDescription().SDP,
	})

	return nil
}

func gotRemoteIce(id string, candidate *webrtc.ICECandidateInit) error {
	if candidate == nil {
		return nil
	}
	conn := connections[id]
	if conn == nil {
		return errors.New("unknown connection")
	}

	return conn.pc.AddICECandidate(*candidate)
}

func gotClose(id string) error {
	conn := connections[id]
	if conn == nil {
		return errors.New("unknown connection")
	}
	return conn.close()
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
		data := unsafe.Slice(
			(*byte)(unsafe.Pointer(unsafe.SliceData(pcm))),
			4*len(pcm),
		)
		_, err := dumpAudioFile.Write(data)
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
			if delta > 10 || packet.Timestamp-nextTS > 48000 {
				// massive packet drop
				debugf("Lost synchronisation, delta=%v", delta)
				buffered = nil
				err := flush(true)
				if err != nil {
					return err
				}
				next = &packet
			} else if delta == 1 {
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
					// discard later packet
					debugf("Out of order packets, "+
						"delta=%v, bdelta=%v",
						delta, bdelta)
					if delta <= bdelta {
						buffered = packet.Clone()
					}
					continue
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
