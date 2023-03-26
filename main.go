//go:build !js
// +build !js

package main

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/pion/dtls/v2"
	"github.com/pion/ice/v2"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

const (
	serializedPeerConnectionsFile = "peerConnections.gob"
	indexHtml                     = `
<html>
  <head>
    <title>webrtc-zero-downtime-reload</title>
  </head>

  <body>
  	<h1 id="statusElement"> </h1>
    <video id="videoElement" controls muted autoplay> </video>
  </body>

  <script>
	const pc = new RTCPeerConnection()
    pc.ontrack = event => {
      videoElement.srcObject = event.streams[0];
    };
	const negotiate = () => {
    	pc.createOffer()
    	.then(offer => {
    	  pc.setLocalDescription(offer)

    	  return fetch('/doSignaling', {
    	    method: 'post',
    	    headers: {
    	      'Accept': 'application/json, text/plain, */*',
    	      'Content-Type': 'application/json'
    	    },
    	    body: JSON.stringify(offer)
    	  })
    	})
    	.then(res => res.json())
    	.then(res => pc.setRemoteDescription(res))
    	.catch(alert)
	}

	fetch('/haveBroadcaster', {
		   headers: {
			 'Accept': 'application/json, text/plain, */*',
		   },
	})
    .then(res => res.json())
	.then(res => {
		if (res.HaveBroadcaster) {
			statusElement.innerText = 'You are viewing';
			pc.addTransceiver('audio', {direction: 'recvonly'})
			pc.addTransceiver('video', {direction: 'recvonly'})
			negotiate()
		} else {
			navigator.mediaDevices.getUserMedia({audio: true, video: true})
			.then(stream => {
				statusElement.innerText = 'You are broadcasting';
	        	videoElement.srcObject = stream;
				stream.getTracks().forEach(t => pc.addTransceiver(t, {direction: 'sendonly'}))
				negotiate()
			})
		}
	})
  </script>
</html>
`
)

type GlobalState struct {
	PeerConnectionState []PeerConnectionState
}

type PeerConnectionState struct {
	RemoteDescription webrtc.SessionDescription

	ICEPort             uint16
	ICEUsernameFragment string
	ICEPassword         string

	DTLSConnectionState dtls.State

	SSRCAudio, SSRCVideo webrtc.SSRC
	SRTPState            map[uint32]uint32
}

var (
	audioTrack, videoTrack *webrtc.TrackLocalStaticRTP
	haveBroadcaster        = atomic.Bool{}
	peerConnections        = []*webrtc.PeerConnection{}
	peerConnectionsMutex   sync.Mutex
)

func main() {
	var err error
	if videoTrack, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "video", "pion"); err != nil {
		panic(err)
	} else if audioTrack, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion"); err != nil {
		panic(err)
	}

	state := GlobalState{}
	if buffer, err := os.ReadFile(serializedPeerConnectionsFile); err == nil {
		dec := gob.NewDecoder(bytes.NewBuffer(buffer))
		if err := dec.Decode(&state); err != nil {
			panic(err)
		}
	}

	deserialize(state)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, indexHtml)
	})
	http.HandleFunc("/doSignaling", doSignaling)
	http.HandleFunc("/haveBroadcaster", func(w http.ResponseWriter, r *http.Request) {
		out := struct {
			HaveBroadcaster bool
		}{haveBroadcaster.Load()}
		json.NewEncoder(w).Encode(&out)
	})

	go func() {
		for range time.NewTicker(2 * time.Second).C {
			serialize()
		}
	}()

	fmt.Println("Open http://localhost:8080 to access this demo")
	panic(http.ListenAndServe(":8080", nil))
}

func doSignaling(w http.ResponseWriter, r *http.Request) {
	s := webrtc.SettingEngine{}
	s.SetSRTPProtectionProfiles(dtls.SRTP_AEAD_AES_128_GCM)
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}

	peerConnection, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(s)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		panic(err)
	}

	peerConnection.OnConnectionStateChange(func(connectionState webrtc.PeerConnectionState) {
		onConnectionStateChangeHandler(peerConnection, connectionState)
	})
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		onTrackHandler(peerConnection, track, receiver)
	})

	var offer webrtc.SessionDescription
	if err = json.NewDecoder(r.Body).Decode(&offer); err != nil {
		panic(err)
	}

	if strings.Contains(offer.SDP, "recvonly") {
		if _, err = peerConnection.AddTrack(videoTrack); err != nil {
			panic(err)
		} else if _, err = peerConnection.AddTrack(audioTrack); err != nil {
			panic(err)
		}
	}

	if err = peerConnection.SetRemoteDescription(offer); err != nil {
		panic(err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	} else if err = peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}
	<-gatherComplete

	response, err := json.Marshal(*peerConnection.LocalDescription())
	if err != nil {
		panic(err)
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(response); err != nil {
		panic(err)
	}
}

func serialize() {
	state := GlobalState{
		PeerConnectionState: []PeerConnectionState{},
	}

	for i := range peerConnections {
		iceTransport := accessUnexported(peerConnections[i], "iceTransport").(*webrtc.ICETransport)
		dtlsTransport := accessUnexported(peerConnections[i], "dtlsTransport").(*webrtc.DTLSTransport)
		dtlsConn := accessUnexported(dtlsTransport, "conn").(*dtls.Conn)
		iceGatherer := accessUnexported(iceTransport, "gatherer").(*webrtc.ICEGatherer)
		iceAgent := accessUnexported(iceGatherer, "agent").(*ice.Agent)

		SSRCVideo, SSRCAudio := webrtc.SSRC(0), webrtc.SSRC(0)

		senders := peerConnections[i].GetSenders()
		for _, sender := range senders {
			encodes := sender.GetParameters().Encodings
			if len(encodes) == 0 {
				panic("Found no encodes")
			}

			if sender.Track().Kind() == webrtc.RTPCodecTypeVideo {
				SSRCVideo = encodes[0].SSRC
			} else {
				SSRCAudio = encodes[0].SSRC
			}

		}

		selectedCandidatePair, err := iceTransport.GetSelectedCandidatePair()
		if err != nil {
			panic(err)
		}

		localUfrag, localPwd, err := iceAgent.GetLocalUserCredentials()
		if err != nil {
			panic(err)
		}

		state.PeerConnectionState = append(state.PeerConnectionState, PeerConnectionState{
			RemoteDescription:   *peerConnections[i].RemoteDescription(),
			ICEPort:             selectedCandidatePair.Local.Port,
			ICEUsernameFragment: localUfrag,
			ICEPassword:         localPwd,
			DTLSConnectionState: dtlsConn.ConnectionState(),
			SSRCAudio:           SSRCAudio,
			SSRCVideo:           SSRCVideo,
			SRTPState:           dtlsTransport.GetSRTPState(),
		})
	}

	var toSave bytes.Buffer
	enc := gob.NewEncoder(&toSave)
	if err := enc.Encode(state); err != nil {
		panic(err)
	}
	if err := os.WriteFile(serializedPeerConnectionsFile, toSave.Bytes(), 0644); err != nil {
		panic(err)
	}
}

func deserialize(state GlobalState) {
	peerConnectionsMutex.Lock()
	defer peerConnectionsMutex.Unlock()

	fmt.Printf("Resuming %d sessions from '%s'\n", len(state.PeerConnectionState), serializedPeerConnectionsFile)

	for i := range state.PeerConnectionState {
		m := &webrtc.MediaEngine{}
		if err := m.RegisterDefaultCodecs(); err != nil {
			panic(err)
		}

		s := webrtc.SettingEngine{}
		s.SetSRTPProtectionProfiles(dtls.SRTP_AEAD_AES_128_GCM)
		s.SetICECredentials(state.PeerConnectionState[i].ICEUsernameFragment, state.PeerConnectionState[i].ICEPassword)
		if err := s.SetEphemeralUDPPortRange(state.PeerConnectionState[i].ICEPort, state.PeerConnectionState[i].ICEPort); err != nil {
			panic(err)
		}
		s.SetDTLSConnectionState(&state.PeerConnectionState[i].DTLSConnectionState)
		s.SetSRTPState(state.PeerConnectionState[i].SRTPState)

		peerConnection, err := webrtc.NewAPI(webrtc.WithSettingEngine(s), webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			panic(err)
		}
		peerConnection.OnConnectionStateChange(func(connectionState webrtc.PeerConnectionState) {
			onConnectionStateChangeHandler(peerConnection, connectionState)
		})
		peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
			onTrackHandler(peerConnection, track, receiver)
		})

		if strings.Contains(state.PeerConnectionState[i].RemoteDescription.SDP, "recvonly") {
			if _, err = peerConnection.AddTransceiverFromTrack(videoTrack, webrtc.RTPTransceiverInit{
				Direction:    webrtc.RTPTransceiverDirectionSendonly,
				SSRCOverride: state.PeerConnectionState[i].SSRCVideo,
			}); err != nil {
				panic(err)
			} else if _, err = peerConnection.AddTransceiverFromTrack(audioTrack, webrtc.RTPTransceiverInit{
				Direction:    webrtc.RTPTransceiverDirectionSendonly,
				SSRCOverride: state.PeerConnectionState[i].SSRCAudio,
			}); err != nil {
				panic(err)
			}
		}

		if err := peerConnection.SetRemoteDescription(state.PeerConnectionState[i].RemoteDescription); err != nil {
			panic(err)
		}

		answer, err := peerConnection.CreateAnswer(nil)
		if err != nil {
			panic(err)
		} else if err = peerConnection.SetLocalDescription(answer); err != nil {
			panic(err)
		}

	}
}

func onConnectionStateChangeHandler(peerConnection *webrtc.PeerConnection, connectionState webrtc.PeerConnectionState) {
	peerConnectionsMutex.Lock()
	defer peerConnectionsMutex.Unlock()

	fmt.Printf("PeerConnection is now: %s\n", connectionState)

	if connectionState == webrtc.PeerConnectionStateFailed {
		n := 0
		for _, savedPeerConnection := range peerConnections {
			if savedPeerConnection != peerConnection {
				peerConnections[n] = savedPeerConnection
				n++
			}
		}
		peerConnections = peerConnections[:n]
		peerConnection.Close()
	} else if connectionState == webrtc.PeerConnectionStateConnected {
		peerConnections = append(peerConnections, peerConnection)
	}

	if connectionState == webrtc.PeerConnectionStateFailed || connectionState == webrtc.PeerConnectionStateConnected {
		serialize()
	}
}

func onTrackHandler(peerConnection *webrtc.PeerConnection, track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	haveBroadcaster.Store(true)
	go func() {
		ticker := time.NewTicker(time.Millisecond * 200)
		for range ticker.C {
			errSend := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}})
			if errSend != nil {
				return
			}
		}
	}()

	outputTrack := videoTrack
	if strings.HasPrefix(track.Codec().MimeType, "audio") {
		outputTrack = audioTrack
	}

	for {
		// Read RTP packets being sent to Pion
		rtp, _, readErr := track.ReadRTP()
		if errors.Is(readErr, io.EOF) {
			return
		} else if readErr != nil {
			panic(readErr)
		}

		if writeErr := outputTrack.WriteRTP(rtp); writeErr != nil {
			panic(writeErr)
		}
	}
}

func accessUnexported(object any, field string) any {
	v := reflect.ValueOf(object).Elem().FieldByName(field)
	return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Interface()
}
