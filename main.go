package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
	"os"
	"sync"
	"errors"
	"io"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/ivfreader"
	"github.com/gorilla/websocket"
	"github.com/pborman/getopt/v2"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:    4096,
	WriteBufferSize:   4096,
	EnableCompression: false,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

var interfaces []string = nil
var httpaddr string = "127.0.0.1"
var httpport uint16 = 8080
var rtpaddr string = "0.0.0.0"
var rtpport uint16 = 8514
var ipv6 bool = false
var help bool = false

type Ping struct {
	Ping bool `json:"ping"`
}

type Pong struct {
	Pong bool `json:"pong"`
}

func parseFrame(frame []byte) (bool, error) {
	// VP9
	if frame[0] & 0b11000000 != 0b10000000 {
		return false, errors.New("not a vp9 frame")
	}

	show_existing_frame := frame[0] & 0b00001000
	frame_type := frame[0] & 0b00000100

	// if profile == 3
	if frame[0] & 0b00110000 == 0b00110000 {
		show_existing_frame = frame_type
		frame_type = frame[0] & 0b00000010
	}

	if show_existing_frame != 0 {
		return false, errors.New("show_existing_frame == true")
	}

	return frame_type == 0, nil
}

func websocketWrite(c *websocket.Conn, m sync.Mutex, v interface{}) error {
	m.Lock()
	defer m.Unlock()
	return c.WriteJSON(v)
}

func handleWebsocket(conn *websocket.Conn, api *webrtc.API, bs *broadcastService) {
	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer peerConnection.Close()

	// Create websocket mutex
	var m sync.Mutex

	// Keep alive websocket
	go func() {
		ping := Ping{true}
		for range time.Tick(time.Second * 5) {
			if err := websocketWrite(conn, m, ping); err != nil {
				return
			}
		}
	}()

	// When Pion gathers a new ICE Candidate send it to the client.
	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		
		if err = websocketWrite(conn, m, candidate.ToJSON()); err != nil {
			fmt.Println(err)
			return
		}
	})

	// Add video track
	videoTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9}, "video", "pion")
	if err != nil {
		fmt.Println(err)
		return
	}

	rtpSender, err := peerConnection.AddTrack(videoTrack)
	if err != nil {
		fmt.Println(err)
		return
	}

	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	// frames channel
	var ch chan []byte = nil

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		//fmt.Printf("ICE Connection State has changed: %s\n", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateConnected {
			if ch != nil {
				return
			}

			ch = bs.add()

			go func() {
				stream := false

				for {
					frame := <- ch

					if frame == nil || len(frame) == 0 {
						bs.del(ch)
						return
					}

					keyFrame, err := parseFrame(frame)

					if err != nil {
						fmt.Println(err)
						return
					}

					if !stream && keyFrame {
						stream = true
					}

					if !stream {
						continue
					}

					//fmt.Println(len(frame))

					if err := videoTrack.WriteSample(media.Sample{Data: frame, Duration: time.Second}); err != nil {
						return
					}
				}
			}()
		} else {
			if ch != nil {
				ch <- nil
			}
		}
	})

	for {
		// Read each inbound WebSocket Message
		_, message, err := conn.ReadMessage()
		if err != nil {
			//fmt.Println(err)
			return
		}

		// Unmarshal each inbound WebSocket message
		var (
			candidate webrtc.ICECandidateInit
			offer     webrtc.SessionDescription
			pong      Pong
		)

		switch {
		// Attempt to unmarshal as a Ping reply
		case json.Unmarshal(message, &pong) == nil && pong.Pong:
			break
		// Attempt to unmarshal as a SessionDescription. If the SDP field is empty
		// assume it is not one.
		case json.Unmarshal(message, &offer) == nil && offer.SDP != "":
			//fmt.Println("SDP:", string(message))
			if err = peerConnection.SetRemoteDescription(offer); err != nil {
				return
			}

			answer, answerErr := peerConnection.CreateAnswer(nil)
			if answerErr != nil {
				return
			}

			if err = peerConnection.SetLocalDescription(answer); err != nil {
				return
			}

			if err = websocketWrite(conn, m, answer); err != nil {
				return
			}
		// Attempt to unmarshal as a ICECandidateInit. If the candidate field is empty
		// assume it is not one.
		case json.Unmarshal(message, &candidate) == nil && candidate.Candidate != "":
			//fmt.Println("Candidate:", string(message))
			if err = peerConnection.AddICECandidate(candidate); err != nil {
				return
			}
		default:
			//fmt.Println("DDD", len(message))
			//fmt.Println(string(message))
			return
		}
	}
}

func interfaceFilter(iface string) bool {
	if len(interfaces) == 0 {
		return true
	}
	for _, i := range interfaces {
		if i == iface {
			return true
		}
	}
	return false
}

func usage() {
	os.Stderr.WriteString(fmt.Sprintf(`Usage: %s [OPTION]... [INTERFACE]...

Options
 -h, --help            display this help and exit
 -6                    add support for ipv6 ice candidates
 -A, --http-addr=IP    httpd listen address, default 127.0.0.1
 -P, --http-port=PORT  httpd listen port, default 8080
 -a, --rtp-addr=IP     rtp listen address, default 0.0.0.0, ::
 -p, --rtp-port=PORT   rtp listen port, default 8514

Interfaces
 list of allowed interfaces for rtp stream, default: allow all

`, os.Args[0]))
}

func init() {
	getopt.Flag(&help, 'h')
	getopt.Flag(&ipv6, '6')
	getopt.FlagLong(&httpaddr, "http-addr", 'A')
	getopt.FlagLong(&httpport, "http-port", 'P')
	getopt.FlagLong(&rtpport, "rtp-addr", 'a')
	getopt.FlagLong(&rtpport, "rtp-port", 'p')
	getopt.SetUsage(usage)
}

func main() {
	getopt.Parse()
	interfaces = getopt.Args()

	if help {
		usage()
		os.Exit(0)
	}

	s := webrtc.SettingEngine{}

	// Enable support only for TCP ICE candidates.
	if ipv6 {
		s.SetNetworkTypes([]webrtc.NetworkType{
			webrtc.NetworkTypeTCP4,
			webrtc.NetworkTypeTCP6,
		})
	} else {
		s.SetNetworkTypes([]webrtc.NetworkType{
			webrtc.NetworkTypeTCP4,
		})
	}

	s.SetInterfaceFilter(interfaceFilter)

	tcpListener, err := net.ListenTCP("tcp", &net.TCPAddr{
		IP:   net.ParseIP(rtpaddr),
		Port: int(rtpport),
	})
	if err != nil {
		panic(err)
	}

	//fmt.Printf("Listening for ICE TCP at %s\n", tcpListener.Addr())

	tcpMux := webrtc.NewICETCPMux(nil, tcpListener, 8)
	s.SetICETCPMux(tcpMux)

	// Create a MediaEngine object to configure the supported codec
	m := &webrtc.MediaEngine{}

	// Use a VP9
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, Channels: 0, SDPFmtpLine: "", RTCPFeedback: nil},
	}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	// Create a InterceptorRegistry
	i := &interceptor.Registry{}

	// Use the default set of Interceptors
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		panic(err)
	}

	// Create broadcast service
	bs := newBroadcastService()
	go bs.run()

	// Create the API object with the MediaEngine and SettingEngine
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i), webrtc.WithSettingEngine(s))

	http.Handle("/", http.FileServer(http.Dir(".")))
	http.HandleFunc("/ice", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			fmt.Println("Upgrade error:", err)
			return
		}
		handleWebsocket(conn, api, bs)
		conn.Close()
	})

	//fmt.Println("Open http://localhost:8099 to access this demo")
	go func() {
		panic(http.ListenAndServe(fmt.Sprintf("%s:%d", httpaddr, int(httpport)), nil))
	}()

	// Read frame loop
	ivf, header, err := ivfreader.NewWith(os.Stdin)
	if err != nil {
		panic(err)
	}

	if header.FourCC != "VP90" {
		fmt.Fprintln(os.Stderr, "Unknown codec: %s, only vp9 supported", header.FourCC)
		os.Exit(1)
	}

	for {
		frame, _, err := ivf.ParseNextFrame()
		if errors.Is(err, io.EOF) {
			fmt.Println("All video frames parsed and sent")
			os.Exit(0)
		}

		if err != nil {
			panic(err)
		}

		bs.broadcast <- frame
	}
}
