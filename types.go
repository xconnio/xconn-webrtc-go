package xconnwebrtc

import (
	"fmt"
	"net"

	"github.com/pion/webrtc/v4"

	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/wampproto-go/serializers"
	"github.com/xconnio/xconn-go"
)

type (
	ICEServer      = webrtc.ICEServer
	CredentialType = webrtc.ICECredentialType
)

const (
	ICECredentialTypePassword = webrtc.ICECredentialTypePassword
	ICECredentialTypeOauth    = webrtc.ICECredentialTypeOauth
)

type Answer struct {
	Candidates  []webrtc.ICECandidateInit `json:"candidates"`
	Description webrtc.SessionDescription `json:"description"`
}

type Offer = Answer

type OfferResponse struct {
	RequestID string `json:"requestID"`
	Answer    Answer `json:"answer"`
}

type OfferConfig struct {
	Protocol                 string
	ICEServers               []webrtc.ICEServer
	Ordered                  bool
	ID                       uint16
	TopicAnswererOnCandidate string
}

type AnswerConfig struct {
	ICEServers []webrtc.ICEServer
}

type ProviderConfig struct {
	Session                     *xconn.Session
	ProcedureHandleOffer        string
	TopicHandleRemoteCandidates string
	TopicPublishLocalCandidate  string
	Serializer                  serializers.Serializer
	Router                      *xconn.Router
	Authenticator               auth.ServerAuthenticator
	ICEServers                  []webrtc.ICEServer
}

func cloneICEServers(servers []webrtc.ICEServer) []webrtc.ICEServer {
	if len(servers) == 0 {
		return nil
	}

	cloned := make([]webrtc.ICEServer, len(servers))
	copy(cloned, servers)

	return cloned
}

func (c *ProviderConfig) validate() error {
	if c == nil {
		return fmt.Errorf("provider config is nil")
	}
	if c.Session == nil {
		return fmt.Errorf("session must not be nil")
	}
	if c.ProcedureHandleOffer == "" {
		return fmt.Errorf("procedureHandleOffer must not be empty")
	}
	if c.TopicHandleRemoteCandidates == "" {
		return fmt.Errorf("topicHandleRemoteCandidates must not be empty")
	}
	if c.TopicPublishLocalCandidate == "" {
		return fmt.Errorf("topicPublishLocalCandidate must not be empty")
	}
	if c.Serializer == nil {
		c.Serializer = &serializers.JSONSerializer{}
	}
	return nil
}

type WebRTCSession struct {
	Connection *webrtc.PeerConnection
	Channel    *webrtc.DataChannel
}

func (w *WebRTCSession) OpenChannel(label string, options *webrtc.DataChannelInit) (*webrtc.DataChannel, error) {
	return w.Connection.CreateDataChannel(label, options)
}

func outboundIPs() []net.IP {
	var ips []net.IP
	if conn, err := net.Dial("udp4", "8.8.8.8:80"); err == nil {
		ips = append(ips, conn.LocalAddr().(*net.UDPAddr).IP)
		conn.Close()
	}
	if conn, err := net.Dial("udp6", "[2001:4860:4860::8888]:80"); err == nil {
		ips = append(ips, conn.LocalAddr().(*net.UDPAddr).IP)
		conn.Close()
	}
	return ips
}

func NewFilteredPeerConnection(iceServers []webrtc.ICEServer) (*webrtc.PeerConnection, error) {
	config := webrtc.Configuration{
		ICEServers:           iceServers,
		ICECandidatePoolSize: 10,
	}

	s := webrtc.SettingEngine{}

	routable := outboundIPs()
	if len(routable) > 0 {
		s.SetIPFilter(func(ip net.IP) bool {
			for _, routableIP := range routable {
				if ip.Equal(routableIP) {
					return true
				}
			}
			return false
		})
	}

	api := webrtc.NewAPI(webrtc.WithSettingEngine(s))

	return api.NewPeerConnection(config)
}
