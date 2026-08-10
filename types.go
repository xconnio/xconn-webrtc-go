package xconnwebrtc

import (
	"fmt"
	"net"
	"slices"
	"time"

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
	// Serializer is unused: each WAMP session now negotiates its own
	// serializer via a RawSocket-style magic-byte handshake on its DataChannel
	// (see Answerer.OnWAMPDataChannel), matching what the client sends via
	// OpenSessionConfig.Serializer / ClientConfig.Serializer. Kept for
	// backwards API compatibility.
	Serializer    serializers.Serializer
	Router        *xconn.Router
	Authenticator auth.ServerAuthenticator
	ICEServers    []webrtc.ICEServer
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

// OpenSessionConfig configures an additional WAMP session opened via WebRTCSession.OpenSession.
type OpenSessionConfig struct {
	Serializer    xconn.SerializerSpec
	Authenticator auth.ClientAuthenticator
	OpenTimeout   time.Duration
}

func (c *OpenSessionConfig) validate() {
	if c.Serializer == nil {
		c.Serializer = xconn.JSONSerializerSpec
	}
	if c.Authenticator == nil {
		c.Authenticator = auth.NewAnonymousAuthenticator("", nil)
	}
	if c.OpenTimeout <= 0 {
		c.OpenTimeout = 20 * time.Second
	}
}

// WebRTCSession is a WAMP session established over a WebRTC DataChannel. It
// extends *xconn.Session, so every WAMP operation (Call, Register, Publish,
// Subscribe, ...) is available directly on it, while also giving access to
// the shared PeerConnection: OpenSession opens more independent WAMP sessions
// on it, and OpenChannel/OnDataChannel open or receive raw (non-WAMP) data
// channels on it. Mirrors xconn.QUICSession for QUIC connections, where many
// sessions and raw streams can share one underlying connection.
type WebRTCSession struct {
	*xconn.Session

	connection *webrtc.PeerConnection
	channel    *webrtc.DataChannel
}

// Connection returns the underlying PeerConnection, shared across every WAMP
// session and raw data channel opened on it.
func (w *WebRTCSession) Connection() *webrtc.PeerConnection {
	return w.connection
}

// OpenChannel opens a new raw (non-WAMP) data channel over the same
// PeerConnection. The label identifies the channel's purpose (e.g. "file-transfer").
func (w *WebRTCSession) OpenChannel(label string, options *webrtc.DataChannelInit) (*webrtc.DataChannel, error) {
	return w.connection.CreateDataChannel(label, options)
}

// OnDataChannel registers a callback for raw (non-WAMP) data channels the
// remote peer opens.
func (w *WebRTCSession) OnDataChannel(callback func(channel *webrtc.DataChannel)) {
	w.connection.OnDataChannel(callback)
}

// OpenSession opens an additional, independent WAMP session on the same
// PeerConnection by creating a new DataChannel. No new offer/answer/ICE
// exchange is required: opening extra data channels on an already-established
// connection is handled by SCTP directly. Mirrors xconn.QUICSession.OpenSession
// for QUIC connections, where many sessions can share one underlying connection.
func (w *WebRTCSession) OpenSession(realm string, config *OpenSessionConfig) (*WebRTCSession, error) {
	if config == nil {
		config = &OpenSessionConfig{}
	}
	config.validate()

	ordered := true
	channel, err := w.connection.CreateDataChannel("data", &webrtc.DataChannelInit{
		Ordered: &ordered,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create data channel: %w", err)
	}

	ready := make(chan struct{})
	channel.OnOpen(func() {
		close(ready)
	})

	timer := time.NewTimer(config.OpenTimeout)
	defer timer.Stop()

	select {
	case <-ready:
	case <-timer.C:
		_ = channel.Close()
		return nil, fmt.Errorf("timed out waiting for data channel to open")
	}

	return joinWebRTCSession(w.connection, channel, realm, config.Serializer, config.Authenticator, config.OpenTimeout)
}

// Close leaves the WAMP session (sending GOODBYE) and closes this session's
// own DataChannel. Other sessions and raw channels sharing the same
// PeerConnection are unaffected.
func (w *WebRTCSession) Close() error {
	err := w.Leave()
	_ = w.channel.Close()
	return err
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
			return slices.ContainsFunc(routable, ip.Equal)
		})
	}

	api := webrtc.NewAPI(webrtc.WithSettingEngine(s))

	return api.NewPeerConnection(config)
}
