package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const windowSize = 50

func doSignaling(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	if r.Method == "OPTIONS" || r.Method == "DELETE" {
		return
	}

	offer, err := io.ReadAll(r.Body)
	if err != nil {
		panic(err)
	}

	s := webrtc.SettingEngine{}
	s.DisableSRTPReplayProtection(true)
	s.DisableSRTCPReplayProtection(true)

	peerConnection, err := webrtc.NewAPI(webrtc.WithSettingEngine(s)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		panic(err)
	}

	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) { //nolint: revive
		if strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeH264) {
			totalSeen := uint16(0)
			largestGapRTXed := uint16(0)
			var pktNacked *rtp.Packet
			wasNacked := []uint16{}

			for {
				pkt, _, err := track.ReadRTP()
				if errors.Is(err, io.EOF) {
					return
				} else if err != nil {
					panic(err)
				}

				totalSeen++
				if pktNacked != nil && pktNacked.Header.SequenceNumber == pkt.Header.SequenceNumber {
					if !bytes.Equal(pktNacked.Payload, pkt.Payload) {
						panic("RTXed packed Payload mismatch")
					}

					fmt.Printf("Pkt with SequenceNumber(%d) correctly RTXed\n", pkt.Header.SequenceNumber)
					largestGapRTXed += windowSize
					pktNacked = nil
					continue
				}

				// Don't attempt to run a NACK test against RTX
				if slices.Contains(wasNacked, pkt.SequenceNumber) {
					continue
				}

				// No NACK test is in flight, start a new one
				if pktNacked == nil {
					pktNacked = pkt
					wasNacked = append(wasNacked, pkt.SequenceNumber)
				}

				nackDistance := (windowSize + largestGapRTXed)
				currentDistance := pkt.SequenceNumber - pktNacked.SequenceNumber

				// Don't send a NACK yet, not far enough
				if currentDistance < nackDistance {
					continue
				} else if currentDistance == nackDistance {
					fmt.Printf("Sending NACK for Packet (%d) which is (%d) in the past\n", pktNacked.Header.SequenceNumber, currentDistance)
				}

				nack := &rtcp.TransportLayerNack{
					SenderSSRC: uint32(track.SSRC()),
					MediaSSRC:  uint32(track.SSRC()),
					Nacks:      rtcp.NackPairsFromSequenceNumbers([]uint16{pktNacked.SequenceNumber}),
				}

				if err := peerConnection.WriteRTCP([]rtcp.Packet{nack}); err != nil {
					panic(err)
				}
			}
		}
	})

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("ICE Connection State has changed: %s\n", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateFailed {
			peerConnection.Close()
		}
	})

	if err = peerConnection.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(offer)}); err != nil {
		panic(err)
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	} else if err = peerConnection.SetLocalDescription(answer); err != nil {
		fmt.Println(answer.SDP)
		panic(err)
	}

	<-gatherComplete

	w.Header().Add("Location", "/")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprint(w, peerConnection.LocalDescription().SDP)
}

func main() {
	rand.Seed(time.Now().UnixNano()) //nolint

	http.HandleFunc("/", doSignaling)

	fmt.Println("Running WHIP server at http://localhost:8085")
	// nolint: gosec
	panic(http.ListenAndServe(":8085", nil))
}
