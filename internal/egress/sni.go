package egress

import "errors"

const (
	recordTypeHandshake  = 0x16
	handshakeClientHello = 0x01
	extensionServerName  = 0x00
	sniHostName          = 0x00
)

var errNoSNI = errors.New("egress: client hello has no server name")

// parseSNI extracts the server_name (SNI) from a TLS ClientHello handshake
// body. It performs strict bounds checking and never panics on malformed input.
func parseSNI(b []byte) (string, error) {
	// Handshake header: msg_type(1) length(3).
	if len(b) < 4 || b[0] != handshakeClientHello {
		return "", errNotTLS
	}
	p := 4
	// legacy_version(2) + random(32).
	if len(b) < p+34 {
		return "", errNoSNI
	}
	p += 34
	// legacy_session_id.
	if len(b) < p+1 {
		return "", errNoSNI
	}
	p += 1 + int(b[p])
	// cipher_suites.
	if len(b) < p+2 {
		return "", errNoSNI
	}
	p += 2 + (int(b[p])<<8 | int(b[p+1]))
	// legacy_compression_methods.
	if len(b) < p+1 {
		return "", errNoSNI
	}
	p += 1 + int(b[p])
	// extensions.
	if len(b) < p+2 {
		return "", errNoSNI
	}
	extEnd := p + 2 + (int(b[p])<<8 | int(b[p+1]))
	p += 2
	if extEnd > len(b) {
		extEnd = len(b)
	}
	for p+4 <= extEnd {
		extType := int(b[p])<<8 | int(b[p+1])
		extLen := int(b[p+2])<<8 | int(b[p+3])
		p += 4
		if p+extLen > len(b) {
			break
		}
		if extType == extensionServerName {
			return parseServerName(b[p : p+extLen])
		}
		p += extLen
	}
	return "", errNoSNI
}

func parseServerName(b []byte) (string, error) {
	if len(b) < 2 {
		return "", errNoSNI
	}
	p := 2
	end := p + (int(b[0])<<8 | int(b[1]))
	if end > len(b) {
		end = len(b)
	}
	for p+3 <= end {
		nameType := b[p]
		nameLen := int(b[p+1])<<8 | int(b[p+2])
		p += 3
		if p+nameLen > len(b) {
			break
		}
		if nameType == sniHostName {
			return string(b[p : p+nameLen]), nil
		}
		p += nameLen
	}
	return "", errNoSNI
}
