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
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"gopkg.in/hraban/opus.v2"
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

type connection struct {
	id string
	pc *webrtc.PeerConnection
}

var connections = make(map[string]*connection)
var writer *messageWriter[*clientMessage]
var api *webrtc.API
var modelFilename string

func main() {
	var password string
	var insecure bool

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr,
			"Usage: %s group [file...]\n", os.Args[0],
		)
		flag.PrintDefaults()
	}
	flag.StringVar(&modelFilename, "model", "models/ggml-base.en.bin",
		"whisper model `filename`")
	flag.StringVar(&username, "username", "speech-to-text",
		"`username` to use for login")
	flag.StringVar(&password, "password", "",
		"`password` to use for login")
	flag.BoolVar(&insecure, "insecure", false,
		"don't check server certificates")
	flag.BoolVar(&debug, "debug", false,
		"enable protocol logging")
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}

	var me webrtc.MediaEngine
	err := me.RegisterDefaultCodecs()
	if err != nil {
		log.Fatalf("RegisterDefaultCodecs: %v", err)
	}
	api = webrtc.NewAPI(webrtc.WithMediaEngine(&me))

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
	writer = newWriter[*clientMessage]()
	go writerLoop(ws, writer)

	readerCh := make(chan *clientMessage, 1)
	go readerLoop(ws, readerCh)

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

func newWriter[T any]() *messageWriter[T] {
	return &messageWriter[T]{
		ch:   make(chan T, 8),
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

	go rtpLoop(track, receiver)
}

const overlapSamples = 200 * 16
const silenceSamples = 300 * 16
const minSamples = 16000
const maxSamples = 3 * 16000

func rtpLoop(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	decoder, err := opus.NewDecoder(16000, 1)
	if err != nil {
		log.Printf("%v", err)
		return
	}
	buf := make([]byte, 2048)
	out := make([]float32, 0, 2*maxSamples)
	var lastSeqno uint16
	var nextTS uint32

	var packet rtp.Packet

	var busy sync.Mutex

	wContext := whisperInit(modelFilename)
	if wContext == nil {
		log.Printf("whisper failure")
		return
	}
	defer func() {
		// wait for the workder to be done before freeing
		busy.Lock()
		busy.Unlock()
		whisperClose(wContext)
	}()

	flush := func(all bool) {
		if len(out) <= overlapSamples {
			if all {
				out = out[:0]
			}
			return
		}
		ok := busy.TryLock()
		if !ok {
			log.Printf("Dropping %v samples", len(out))
			out = out[:0]
			return
		}
		go func(out []float32) {
			for len(out) < minSamples {
				out = append(out, 0.0)
			}
			whisper(wContext, out)
			busy.Unlock()
		}(append([]float32(nil), out...))
		if all {
			out = out[:0]
		} else {
			copy(out, out[len(out)-overlapSamples:])
			out = out[:overlapSamples]
		}
	}

	for {
		bytes, _, err := track.Read(buf)
		if err != nil {
			if err != io.EOF {
				log.Printf("%v", err)
			}
			flush(true)
			return
		}
		err = packet.Unmarshal(buf[:bytes])
		if err != nil {
			log.Printf("%v", err)
			continue
		}

		if len(out) > 0 {
			delta := packet.SequenceNumber - lastSeqno
			if delta == 0 || delta >= 0xFF00 {
				// late packet, drop it
				continue
			}
			if delta == 2 && nextTS-packet.Timestamp <= 4800 {
				// packet loss concealement
				samples := int(nextTS - packet.Timestamp/3)
				err := decoder.DecodePLCFloat32(
					out[len(out) : len(out)+samples],
				)
				if err == nil {
					out = out[:len(out)+samples]
					lastSeqno++
					delta--
					nextTS = packet.Timestamp
				} else {
					log.Printf("PLC: %v", err)
				}
			}
			if delta != 1 {
				flush(true)
			}
		}

		n, err := decoder.DecodeFloat32(
			packet.Payload, out[len(out):cap(out)],
		)
		if err != nil {
			log.Printf("Decode: %v", err)
			flush(true)
			continue
		}

		out = out[:len(out)+n]
		lastSeqno = packet.SequenceNumber
		nextTS = packet.Timestamp + uint32(3*n)

		if len(out) > minSamples {
			s := float32(0.0)
			for _, v := range out[len(out)-silenceSamples:] {
				s += v * v
			}
			s = s / float32(silenceSamples)
			if s < 1e-4 {
				flush(true)
			}
		} else if len(out) > maxSamples {
			flush(false)
		}
	}
}
