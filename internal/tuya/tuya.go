// Package tuya implements just enough of the Tuya local LAN protocol to read a metering plug's data points:
// protocol 3.3 (AES-128-ECB, CRC32 frames), 3.4 (session-key negotiation, ECB with the session key, HMAC-SHA256
// frames) and 3.5 (session-key negotiation, AES-128-GCM "6699" frames).
//
// Written against tinytuya (github.com/jasonacox/tinytuya, core/message_helper.py, crypto_helper.py,
// XenonDevice.py at commit c0eb2ee, 2026-10-28). Frame encoding is checked against byte vectors generated with
// tinytuya's own pack_message/AESCipher (see tuya_test.go). Talking to real 3.4/3.5 hardware is NOT verified here.
package tuya

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Command types (tinytuya core/command_types.py).
const (
	SessKeyNegStart  = 3
	SessKeyNegResp   = 4
	SessKeyNegFinish = 5
	Control          = 7
	Status           = 8
	HeartBeat        = 9
	DPQuery          = 0x0a
	ControlNew       = 0x0d
	DPQueryNew       = 0x10
	UpdateDPS        = 0x12
	LanExtStream     = 0x40

	prefix55AA = 0x000055AA
	suffix55AA = 0x0000AA55
	prefix6699 = 0x00006699
	suffix6699 = 0x00009966
	maxPayload = 4096 * 4
)

// Commands that never carry the "3.x" + 12 zero bytes protocol header.
var noHeaderCmds = map[uint32]bool{DPQuery: true, DPQueryNew: true, UpdateDPS: true, HeartBeat: true,
	SessKeyNegStart: true, SessKeyNegResp: true, SessKeyNegFinish: true, LanExtStream: true}

var ErrDecode = errors.New("tuya: decode error")

// ---------------------------------------------------------------- crypto
func pkcs7Pad(b []byte) []byte {
	n := 16 - len(b)%16
	return append(append([]byte{}, b...), bytes.Repeat([]byte{byte(n)}, n)...)
}

func pkcs7Unpad(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, ErrDecode
	}
	n := int(b[len(b)-1])
	if n < 1 || n > 16 || n > len(b) {
		return nil, ErrDecode
	}
	return b[:len(b)-n], nil
}

// EncryptECB: AES-128-ECB (Tuya's choice, not ours), PKCS#7-padded when pad is true.
func EncryptECB(key, data []byte, pad bool) ([]byte, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if pad {
		data = pkcs7Pad(data)
	}
	if len(data)%16 != 0 {
		return nil, ErrDecode
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += 16 {
		blk.Encrypt(out[i:i+16], data[i:i+16])
	}
	return out, nil
}

func DecryptECB(key, data []byte, unpad bool) ([]byte, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data)%16 != 0 {
		return nil, ErrDecode
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += 16 {
		blk.Decrypt(out[i:i+16], data[i:i+16])
	}
	if unpad {
		return pkcs7Unpad(out)
	}
	return out, nil
}

func gcmSeal(key, iv, plain, aad []byte) ([]byte, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(blk) // 12-byte nonce, 16-byte tag
	if err != nil {
		return nil, err
	}
	return g.Seal(nil, iv, plain, aad), nil // ciphertext || tag
}

func gcmOpen(key, iv, ctAndTag, aad []byte) ([]byte, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return g.Open(nil, iv, ctAndTag, aad)
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// ---------------------------------------------------------------- frames
// Pack55AA: prefix, seqno, cmd, length (BE uint32) | payload | crc32 or hmac-sha256 | suffix.
func Pack55AA(seq, cmd uint32, payload, hmacKey []byte) []byte {
	endLen := 8
	if hmacKey != nil {
		endLen = 36
	}
	buf := make([]byte, 16, 16+len(payload)+endLen)
	binary.BigEndian.PutUint32(buf[0:], prefix55AA)
	binary.BigEndian.PutUint32(buf[4:], seq)
	binary.BigEndian.PutUint32(buf[8:], cmd)
	binary.BigEndian.PutUint32(buf[12:], uint32(len(payload)+endLen))
	buf = append(buf, payload...)
	if hmacKey != nil {
		buf = append(buf, hmacSHA256(hmacKey, buf)...)
	} else {
		buf = binary.BigEndian.AppendUint32(buf, crc32.ChecksumIEEE(buf))
	}
	return binary.BigEndian.AppendUint32(buf, suffix55AA)
}

// Pack6699: prefix(4) unknown(2)=0 seqno(4) cmd(4) length(4) | iv(12) | AES-GCM(ciphertext) | tag(16) | suffix.
// The 14 header bytes after the prefix are the GCM additional data.
func Pack6699(seq, cmd uint32, payload, key, iv []byte) ([]byte, error) {
	if len(iv) != 12 {
		return nil, errors.New("tuya: iv must be 12 bytes")
	}
	hdr := make([]byte, 18)
	binary.BigEndian.PutUint32(hdr[0:], prefix6699)
	binary.BigEndian.PutUint16(hdr[4:], 0)
	binary.BigEndian.PutUint32(hdr[6:], seq)
	binary.BigEndian.PutUint32(hdr[10:], cmd)
	binary.BigEndian.PutUint32(hdr[14:], uint32(len(payload)+28))
	sealed, err := gcmSeal(key, iv, payload, hdr[4:])
	if err != nil {
		return nil, err
	}
	out := append(hdr, iv...)
	out = append(out, sealed...)
	return binary.BigEndian.AppendUint32(out, suffix6699), nil
}

// Message is a decoded frame.
type Message struct {
	Seq, Cmd, Retcode uint32
	Payload           []byte
	CRCGood           bool
	Prefix            uint32
}

// FrameLength returns the full length of the frame starting at data[0] (needs the header bytes), or 0.
func FrameLength(data []byte) (int, error) {
	if len(data) < 4 {
		return 0, nil
	}
	switch binary.BigEndian.Uint32(data) {
	case prefix55AA:
		if len(data) < 16 {
			return 0, nil
		}
		n := binary.BigEndian.Uint32(data[12:])
		if n > maxPayload {
			return 0, ErrDecode
		}
		return 16 + int(n), nil
	case prefix6699:
		if len(data) < 18 {
			return 0, nil
		}
		n := binary.BigEndian.Uint32(data[14:])
		if n > maxPayload {
			return 0, ErrDecode
		}
		return 18 + int(n) + 4, nil
	}
	return 0, fmt.Errorf("%w: bad prefix", ErrDecode)
}

// Unpack decodes one frame. For 55AA frames the 4-byte return code is present in device replies (hasRetcode).
// key: HMAC key for 3.4 (nil = CRC32), GCM key for 6699 frames.
func Unpack(data, key []byte, hasRetcode bool) (Message, error) {
	total, err := FrameLength(data)
	if err != nil {
		return Message{}, err
	}
	if total == 0 || len(data) < total {
		return Message{}, fmt.Errorf("%w: short frame", ErrDecode)
	}
	pfx := binary.BigEndian.Uint32(data)
	if pfx == prefix55AA {
		m := Message{Prefix: pfx, Seq: binary.BigEndian.Uint32(data[4:]), Cmd: binary.BigEndian.Uint32(data[8:])}
		endLen := 8
		if key != nil {
			endLen = 36
		}
		retLen := 0
		if hasRetcode {
			retLen = 4
		}
		if total-16 < retLen+endLen {
			return Message{}, fmt.Errorf("%w: short frame", ErrDecode)
		}
		if retLen > 0 {
			m.Retcode = binary.BigEndian.Uint32(data[16:])
		}
		body := data[:total-endLen]
		m.Payload = append([]byte{}, data[16+retLen:total-endLen]...)
		mac := data[total-endLen : total-4]
		if key != nil {
			m.CRCGood = hmac.Equal(mac, hmacSHA256(key, body))
		} else {
			m.CRCGood = binary.BigEndian.Uint32(mac) == crc32.ChecksumIEEE(body)
		}
		return m, nil
	}
	// 6699
	m := Message{Prefix: pfx, Seq: binary.BigEndian.Uint32(data[6:]), Cmd: binary.BigEndian.Uint32(data[10:])}
	if total < 18+12+16+4 {
		return Message{}, fmt.Errorf("%w: short frame", ErrDecode)
	}
	iv := data[18:30]
	ctTag := data[30 : total-4]
	plain, err := gcmOpen(key, iv, ctTag, data[4:18])
	if err != nil {
		return Message{}, fmt.Errorf("%w: GCM authentication failed", ErrDecode)
	}
	m.CRCGood = true
	if hasRetcode && len(plain) >= 4 {
		m.Retcode = binary.BigEndian.Uint32(plain)
		plain = plain[4:]
	}
	m.Payload = plain
	return m, nil
}

// ---------------------------------------------------------------- device
// Device is one plug. Tuya devices accept a single TCP connection, so it is kept open (persistent) and
// re-established after any error. Not safe for concurrent use (the poller never overlaps a source).
type Device struct {
	ID, IP  string
	Key     []byte // 16-byte local key
	Version string // "3.3", "3.4", "3.5" (3.1/3.2 are treated like 3.3)
	Port    int
	Timeout time.Duration

	mu       sync.Mutex
	conn     net.Conn
	seq      uint32
	session  []byte
	device22 bool
	randIV   func() []byte // tests
}

func NewDevice(id, ip, localKey, version string, timeout time.Duration) (*Device, error) {
	if len(localKey) != 16 {
		return nil, errors.New("tuya: local_key must be 16 characters")
	}
	if version == "" {
		version = "3.3"
	}
	switch version {
	case "3.1", "3.2", "3.3", "3.4", "3.5":
	default:
		return nil, fmt.Errorf("tuya: unsupported protocol version %s", version)
	}
	return &Device{ID: id, IP: ip, Key: []byte(localKey), Version: version, Port: 6668, Timeout: timeout, seq: 1}, nil
}

func (d *Device) v() float64 { f, _ := strconv.ParseFloat(d.Version, 64); return f }

func (d *Device) versionHeader() []byte {
	return append([]byte(d.Version[:3]), make([]byte, 12)...)
}

func (d *Device) key() []byte {
	if d.session != nil {
		return d.session
	}
	return d.Key
}

func (d *Device) iv() []byte {
	if d.randIV != nil {
		return d.randIV()
	}
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return b
}

// Encode wraps a command payload for this device's protocol version (exported for tests).
func (d *Device) Encode(cmd uint32, payload []byte) ([]byte, error) {
	v := d.v()
	seq := d.seq
	d.seq++
	switch {
	case v >= 3.5:
		if !noHeaderCmds[cmd] {
			payload = append(d.versionHeader(), payload...)
		}
		return Pack6699(seq, cmd, payload, d.key(), d.iv())
	case v >= 3.4:
		if !noHeaderCmds[cmd] {
			payload = append(d.versionHeader(), payload...)
		}
		enc, err := EncryptECB(d.key(), payload, true)
		if err != nil {
			return nil, err
		}
		return Pack55AA(seq, cmd, enc, d.key()), nil
	default: // 3.2 / 3.3
		enc, err := EncryptECB(d.Key, payload, true)
		if err != nil {
			return nil, err
		}
		if !noHeaderCmds[cmd] {
			enc = append(d.versionHeader(), enc...)
		}
		return Pack55AA(seq, cmd, enc, nil), nil
	}
}

// DecodePayload turns a received frame's payload into JSON (exported for tests).
func (d *Device) DecodePayload(p []byte) (map[string]any, error) {
	v := d.v()
	var err error
	if v >= 3.4 && v < 3.5 {
		if p, err = DecryptECB(d.key(), p, true); err != nil {
			return nil, fmt.Errorf("%w: decrypt failed (wrong local key?)", ErrDecode)
		}
	}
	if bytes.HasPrefix(p, []byte(d.Version[:3])) && len(p) >= 15 {
		p = p[15:]
	} else if d.device22 && len(p)&0x0f != 0 && len(p) >= 15 {
		p = p[15:]
	}
	if v < 3.4 {
		if p, err = DecryptECB(d.Key, p, true); err != nil {
			return nil, fmt.Errorf("%w: decrypt failed (wrong local key?)", ErrDecode)
		}
	}
	if bytes.Contains(p, []byte("data unvalid")) && !d.device22 {
		d.device22 = true
		return nil, errDevice22
	}
	var out map[string]any
	if err := json.Unmarshal(p, &out); err != nil {
		return nil, fmt.Errorf("%w: invalid JSON", ErrDecode)
	}
	if _, ok := out["dps"]; !ok {
		if data, ok := out["data"].(map[string]any); ok {
			if dps, ok := data["dps"]; ok {
				out["dps"] = dps
			}
		}
	}
	return out, nil
}

var errDevice22 = errors.New("tuya: device22 detected")

// queryPayload: {"gwId","devId","uid","t"} in tinytuya's key order for 3.3; "{}" (DP_QUERY_NEW) for 3.4/3.5.
func (d *Device) queryPayload(now time.Time) (uint32, []byte) {
	if d.v() >= 3.4 {
		return DPQueryNew, []byte("{}")
	}
	t := strconv.FormatInt(now.Unix(), 10)
	id, _ := json.Marshal(d.ID)
	if d.device22 {
		return ControlNew, []byte(fmt.Sprintf(`{"devId":%s,"uid":%s,"t":"%s","dps":{"1":null}}`, id, id, t))
	}
	return DPQuery, []byte(fmt.Sprintf(`{"gwId":%s,"devId":%s,"uid":%s,"t":"%s"}`, id, id, id, t))
}

func (d *Device) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closeLocked()
}

func (d *Device) closeLocked() {
	if d.conn != nil {
		d.conn.Close()
	}
	d.conn, d.session = nil, nil
}

func (d *Device) connect() error {
	if d.conn != nil {
		return nil
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort(d.IP, strconv.Itoa(d.Port)), d.Timeout)
	if err != nil {
		return errors.New("tuya: connection failed")
	}
	d.conn, d.session, d.seq = c, nil, 1
	if d.v() >= 3.4 {
		if err := d.negotiate(); err != nil {
			d.closeLocked()
			return err
		}
	}
	return nil
}

func (d *Device) send(frame []byte) error {
	_ = d.conn.SetDeadline(time.Now().Add(d.Timeout))
	_, err := d.conn.Write(frame)
	return err
}

func (d *Device) recv() (Message, error) {
	_ = d.conn.SetDeadline(time.Now().Add(d.Timeout))
	hdr := make([]byte, 18)
	if _, err := io.ReadFull(d.conn, hdr[:16]); err != nil {
		return Message{}, err
	}
	need := 16
	if binary.BigEndian.Uint32(hdr) == prefix6699 {
		if _, err := io.ReadFull(d.conn, hdr[16:18]); err != nil {
			return Message{}, err
		}
		need = 18
	}
	total, err := FrameLength(hdr[:need])
	if err != nil {
		return Message{}, err
	}
	frame := make([]byte, total)
	copy(frame, hdr[:need])
	if _, err := io.ReadFull(d.conn, frame[need:]); err != nil {
		return Message{}, err
	}
	var key []byte
	if d.v() >= 3.4 {
		key = d.key()
	}
	m, err := Unpack(frame, key, true)
	if err == nil && !m.CRCGood {
		return m, fmt.Errorf("%w: checksum mismatch (wrong local key?)", ErrDecode)
	}
	return m, err
}

// negotiate: 3.4/3.5 session key (tinytuya _negotiate_session_key_*).
func (d *Device) negotiate() error {
	local := make([]byte, 16)
	_, _ = rand.Read(local)
	f, err := d.Encode(SessKeyNegStart, local)
	if err != nil {
		return err
	}
	if err := d.send(f); err != nil {
		return errors.New("tuya: connection failed")
	}
	var resp Message
	for i := 0; i < 2; i++ {
		if resp, err = d.recv(); err != nil {
			return errors.New("tuya: no session key reply (wrong local key / protocol version?)")
		}
		if len(resp.Payload) > 0 {
			break
		}
	}
	if resp.Cmd != SessKeyNegResp {
		return errors.New("tuya: unexpected session key reply")
	}
	p := resp.Payload
	if d.v() < 3.5 {
		if p, err = DecryptECB(d.Key, p, true); err != nil {
			return errors.New("tuya: session key reply decrypt failed (wrong local key?)")
		}
	}
	if len(p) < 48 {
		return errors.New("tuya: session key reply too short")
	}
	remote := p[:16]
	if !hmac.Equal(p[16:48], hmacSHA256(d.Key, local)) {
		return errors.New("tuya: session key HMAC check failed (wrong local key?)")
	}
	f, err = d.Encode(SessKeyNegFinish, hmacSHA256(d.Key, remote))
	if err != nil {
		return err
	}
	if err := d.send(f); err != nil {
		return errors.New("tuya: connection failed")
	}
	d.session, err = SessionKey(d.Key, local, remote, d.v() >= 3.5)
	return err
}

// SessionKey derives the 3.4 / 3.5 session key from both nonces.
func SessionKey(localKey, localNonce, remoteNonce []byte, v35 bool) ([]byte, error) {
	x := make([]byte, 16)
	for i := range x {
		x[i] = localNonce[i] ^ remoteNonce[i]
	}
	if !v35 {
		return EncryptECB(localKey, x, false)
	}
	sealed, err := gcmSeal(localKey, localNonce[:12], x, nil)
	if err != nil {
		return nil, err
	}
	return sealed[:16], nil // ciphertext only (tinytuya takes [12:28] of iv+ct+tag)
}

// Status asks for the device's data points; refresh lists DPS to update first (UPDATEDPS, e.g. 18,19,20).
func (d *Device) Status(refresh []int) (map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.status(refresh)
	if err != nil {
		d.closeLocked() // reconnect next time
	}
	return res, err
}

func (d *Device) status(refresh []int) (map[string]any, error) {
	if err := d.connect(); err != nil {
		return nil, err
	}
	if len(refresh) > 0 {
		ids := make([]string, len(refresh))
		for i, r := range refresh {
			ids[i] = strconv.Itoa(r)
		}
		f, err := d.Encode(UpdateDPS, []byte(`{"dpId":[`+strings.Join(ids, ",")+`]}`))
		if err != nil {
			return nil, err
		}
		if err := d.send(f); err != nil {
			return nil, errors.New("tuya: connection failed")
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		cmd, payload := d.queryPayload(time.Now())
		f, err := d.Encode(cmd, payload)
		if err != nil {
			return nil, err
		}
		if err := d.send(f); err != nil {
			return nil, errors.New("tuya: connection failed")
		}
		deadline := time.Now().Add(d.Timeout)
		for time.Now().Before(deadline) {
			m, err := d.recv()
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					return nil, errors.New("tuya: timeout")
				}
				if errors.Is(err, ErrDecode) {
					return nil, err
				}
				return nil, errors.New("tuya: connection failed")
			}
			if len(m.Payload) == 0 || (m.Cmd != cmd && m.Cmd != Status && m.Cmd != DPQuery && m.Cmd != DPQueryNew && m.Cmd != ControlNew) {
				continue // acks for UPDATEDPS etc.
			}
			out, err := d.DecodePayload(m.Payload)
			if errors.Is(err, errDevice22) {
				break // resend as device22
			}
			if err != nil {
				return nil, err
			}
			if dps, ok := out["dps"].(map[string]any); ok {
				return dps, nil
			}
		}
		if !d.device22 {
			break
		}
	}
	return nil, errors.New("tuya: no dps in reply (wrong local key / protocol version?)")
}
