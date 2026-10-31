package signaling

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

// The intercom negotiates SRTP and hangs up after 30 s without a packet it can
// authenticate, so the keepalive is encrypted with the key our SDP gave it.
func TestDialogKeepaliveEncryptsWithTheKeyOfOurSDP(t *testing.T) {
	t.Parallel()

	local := BuildAnswer("127.0.0.1", "")
	key := audioCryptoKey(t, local)
	receiver, remote := keepaliveReceiver(t, "a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:"+base64.StdEncoding.EncodeToString(make([]byte, 30)))

	startTestKeepalive(t, remote, local)

	decrypt, err := srtp.CreateContext(key[:16], key[16:], srtp.ProtectionProfileAes128CmHmacSha1_80)
	if err != nil {
		t.Fatal(err)
	}

	plain, err := decrypt.DecryptRTP(nil, readKeepalive(t, receiver), nil)
	if err != nil {
		t.Fatalf("DecryptRTP() error = %v, want a packet protected with our key", err)
	}

	var packet rtp.Packet
	if err := packet.Unmarshal(plain); err != nil {
		t.Fatal(err)
	}

	if packet.PayloadType != keepalivePayloadType || string(packet.Payload) != string(keepalivePayload) {
		t.Fatalf("keepalive = PT %d payload %v, want PT %d payload %v", packet.PayloadType, packet.Payload, keepalivePayloadType, keepalivePayload)
	}
}

// A far end that answered without SRTP gets plain RTP.
func TestDialogKeepaliveStaysClearWithoutRemoteCrypto(t *testing.T) {
	t.Parallel()

	receiver, remote := keepaliveReceiver(t, "")

	startTestKeepalive(t, remote, BuildOffer("127.0.0.1", ""))

	var packet rtp.Packet
	if err := packet.Unmarshal(readKeepalive(t, receiver)); err != nil {
		t.Fatal(err)
	}

	if string(packet.Payload) != string(keepalivePayload) {
		t.Fatalf("keepalive payload = %v, want clear %v", packet.Payload, keepalivePayload)
	}
}

func TestParseAudioCryptoReadsTheFirstAudioSection(t *testing.T) {
	t.Parallel()

	key := make([]byte, 30)
	for i := range key {
		key[i] = byte(i)
	}

	sdp := strings.Join([]string{
		"v=0",
		"m=audio 9554 RTP/SAVP 110",
		"a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:" + base64.StdEncoding.EncodeToString(key) + "|2^31",
		"m=video 21922 RTP/SAVP 96",
		"a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:" + base64.StdEncoding.EncodeToString(make([]byte, 30)),
	}, "\r\n")

	got, ok := parseAudioCrypto(sdp)
	if !ok || string(got) != string(key) {
		t.Fatalf("parseAudioCrypto() = %v, %v, want %v, true", got, ok, key)
	}

	for _, sdp := range []string{
		"m=audio 9554 RTP/AVP 110\r\n",
		"m=audio 9554 RTP/SAVP 110\r\na=crypto:1 AES_CM_128_HMAC_SHA1_32 inline:" + base64.StdEncoding.EncodeToString(key) + "\r\n",
		"m=audio 9554 RTP/SAVP 110\r\na=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:dummykey\r\n",
		"m=video 9554 RTP/SAVP 96\r\na=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:" + base64.StdEncoding.EncodeToString(key) + "\r\n",
	} {
		if _, ok := parseAudioCrypto(sdp); ok {
			t.Fatalf("parseAudioCrypto(%q) found a key", sdp)
		}
	}
}

func audioCryptoKey(t *testing.T, sdp string) []byte {
	t.Helper()

	key, ok := parseAudioCrypto(sdp)
	if !ok {
		t.Fatalf("no audio crypto key in %s", sdp)
	}

	return key
}

// keepaliveReceiver stands in for the far end's audio port and returns the SDP
// it answers with, carrying crypto when one is given.
func keepaliveReceiver(t *testing.T, crypto string) (*net.UDPConn, string) {
	t.Helper()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	lines := []string{"v=0", "c=IN IP4 127.0.0.1", fmt.Sprintf("m=audio %d RTP/SAVP 110", conn.LocalAddr().(*net.UDPAddr).Port)}
	if crypto != "" {
		lines = append(lines, crypto)
	}

	return conn, strings.Join(lines, "\r\n") + "\r\n"
}

func startTestKeepalive(t *testing.T, remoteSDP, localSDP string) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	startDialogKeepalive(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), remoteSDP, localSDP, 0, time.Hour)
}

func readKeepalive(t *testing.T, conn *net.UDPConn) []byte {
	t.Helper()

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 1500)

	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("no keepalive received: %v", err)
	}

	return buf[:n]
}
