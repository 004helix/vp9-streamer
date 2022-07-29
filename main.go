package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	_ "runtime"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pborman/getopt/v2"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/ivfreader"
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
	if frame[0]&0b11000000 != 0b10000000 {
		return false, errors.New("not a vp9 frame")
	}

	show_existing_frame := frame[0] & 0b00001000
	frame_type := frame[0] & 0b00000100

	// if profile == 3
	if frame[0]&0b00110000 == 0b00110000 {
		show_existing_frame = frame_type
		frame_type = frame[0] & 0b00000010
	}

	if show_existing_frame != 0 {
		return false, nil
	}

	// VP9 frame_type:
	//  0 - KEY_FRAME
	//  1 - INTER_FRAME
	return frame_type == 0, nil
}

func handleWebsocket(conn *websocket.Conn, api *webrtc.API, bs *broadcastService) {
	// Create websocket writer goroutine + keep alive
	wsSend := make(chan interface{})
	go func() {
		ping := Ping{true}
		tick := time.NewTicker(time.Second * 5)
		defer tick.Stop()

		for {
			select {
			case <-tick.C:
				conn.WriteJSON(ping)
			case v, ok := <-wsSend:
				if !ok {
					return
				}
				if err = conn.WriteJSON(v); err != nil {
					// stop writer on error
					fmt.Fprintln(os.Stderr, err)
					return
				}
			}
		}
	}()
	defer close(wsSend)

	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer peerConnection.Close()

	// When Pion gathers a new ICE Candidate send it to the client.
	peerConnection.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			wsSend <- c.ToJSON()
		}
	})

	// Add video track
	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9},
		"video",
		"pion",
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	rtpSender, err := peerConnection.AddTrack(videoTrack)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	// Read incoming RTCP packets
	// Before these packets are returned they are processed by interceptors.
	// For things like NACK this needs to be called.
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := rtpSender.Read(buf); err != nil {
				return
			}
		}
	}()

	// Input frames channel
	var frames chan []byte = nil

	// Set the handler for ICE connection state
	// This will enable / disable video stream
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		//fmt.Printf("ICE Connection State has changed: %s\n", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateConnected {
			if frames != nil {
				return
			}

			frames = bs.add()

			go func() {
				stream := false

				for {
					frame, ok := <-frames

					if !ok {
						return
					}

					if len(frame) == 0 {
						continue
					}

					keyFrame, err := parseFrame(frame)

					if err != nil {
						fmt.Fprintln(os.Stderr, err)
						continue
					}

					// wait the first key frame
					if !stream && keyFrame {
						stream = true
					}

					if !stream {
						continue
					}

					//fmt.Println(len(frame), runtime.NumGoroutine())

					if err := videoTrack.WriteSample(media.Sample{Data: frame, Duration: time.Second}); err != nil {
						return
					}
				}
			}()

			return
		}

		if frames != nil {
			bs.del(frames)
			frames = nil
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

			answer, err := peerConnection.CreateAnswer(nil)
			if err != nil {
				return
			}

			if err = peerConnection.SetLocalDescription(answer); err != nil {
				return
			}

			wsSend <- answer
		// Attempt to unmarshal as a ICECandidateInit. If the candidate field is empty
		// assume it is not one.
		case json.Unmarshal(message, &candidate) == nil && candidate.Candidate != "":
			//fmt.Println("Candidate:", string(message))
			if err = peerConnection.AddICECandidate(candidate); err != nil {
				return
			}
		default:
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

	// Handle index
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			return
		}

		http.ServeFile(w, r, "index.html")
	})

	// Handle websocket
	http.HandleFunc("/ice", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			fmt.Println("Upgrade error:", err)
			return
		}
		defer conn.Close()

		handleWebsocket(conn, api, bs)
	})

	go func() {
		panic(http.ListenAndServe(fmt.Sprintf("%s:%d", httpaddr, int(httpport)), nil))
	}()

	// Read ivf data from stdin
	ivf, header, err := ivfreader.NewWith(os.Stdin)
	if err != nil {
		panic(err)
	}

	if header.FourCC != "VP90" {
		fmt.Fprintln(os.Stderr, "Unknown codec: %s", header.FourCC)
		os.Exit(1)
	}

	for {
		frame, _, err := ivf.ParseNextFrame()

		if errors.Is(err, io.EOF) {
			//fmt.Println("All video frames parsed and sent")
			os.Exit(0)
		}

		if err != nil {
			panic(err)
		}

		bs.broadcast <- frame
	}
}
