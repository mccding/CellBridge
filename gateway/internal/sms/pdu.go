package sms

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

type Encoding string

const (
	EncodingGSM7 Encoding = "gsm7"
	EncodingUCS2 Encoding = "ucs2"
)

type SubmitSegment struct {
	PDU      string   `json:"pdu"`
	Encoding Encoding `json:"encoding"`
	PartNo   int      `json:"partNo"`
	Total    int      `json:"totalParts"`
	Body     string   `json:"-"`
}

type DecodedMessage struct {
	Sender      string
	Body        string
	Encoding    Encoding
	ConcatRef   uint8
	PartNo      int
	TotalParts  int
	ServiceTime string
}

var ErrInvalidPDU = errors.New("invalid SMS PDU")

// GSM7 is the GSM 03.38 default alphabet. Extension-table characters are
// represented by ESC + extension code and therefore consume two septets.
var gsm7Alphabet = []rune("@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞ\u001bÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà")

var gsm7Extension = map[rune]byte{
	'^': 0x14, '{': 0x28, '}': 0x29, '\\': 0x2f,
	'[': 0x3c, '~': 0x3d, ']': 0x3e, '|': 0x40, '€': 0x65,
}

var gsm7Reverse = func() map[rune]byte {
	result := make(map[rune]byte, len(gsm7Alphabet))
	for index, value := range gsm7Alphabet {
		result[value] = byte(index)
	}
	return result
}()

func CanEncodeGSM7(body string) bool {
	for _, value := range body {
		if _, ok := gsm7Reverse[value]; ok {
			continue
		}
		if _, ok := gsm7Extension[value]; !ok {
			return false
		}
	}
	return true
}

func GSM7Length(body string) int {
	length := 0
	for _, value := range body {
		if _, ok := gsm7Extension[value]; ok {
			length += 2
		} else {
			length++
		}
	}
	return length
}

func EncodeSubmitSegments(destination, body string, concatRef uint8) ([]SubmitSegment, error) {
	if strings.TrimSpace(destination) == "" || body == "" {
		return nil, fmt.Errorf("%w: destination and body are required", ErrInvalidPDU)
	}
	encoding := EncodingUCS2
	if CanEncodeGSM7(body) {
		encoding = EncodingGSM7
	}
	parts := splitBody(body, encoding)
	total := len(parts)
	segments := make([]SubmitSegment, 0, total)
	for index, part := range parts {
		udh := []byte(nil)
		if total > 1 {
			udh = []byte{0x05, 0x00, 0x03, concatRef, byte(total), byte(index + 1)}
		}
		pdu, err := encodeSubmit(destination, part, encoding, udh)
		if err != nil {
			return nil, err
		}
		segments = append(segments, SubmitSegment{PDU: pdu, Encoding: encoding, PartNo: index + 1, Total: total, Body: part})
	}
	return segments, nil
}

func splitBody(body string, encoding Encoding) []string {
	max := 160
	if encoding == EncodingUCS2 {
		max = 70
	}
	if (encoding == EncodingGSM7 && GSM7Length(body) <= max) || (encoding == EncodingUCS2 && utf16Length(body) <= max) {
		return []string{body}
	}
	if encoding == EncodingGSM7 {
		max = 153
	} else {
		max = 67
	}
	var parts []string
	var current []rune
	length := 0
	for _, value := range body {
		cost := 1
		if encoding == EncodingGSM7 {
			if _, ok := gsm7Extension[value]; ok {
				cost = 2
			}
		} else if value > 0xffff {
			cost = 2
		}
		if length+cost > max && len(current) > 0 {
			parts = append(parts, string(current))
			current = nil
			length = 0
		}
		current = append(current, value)
		length += cost
	}
	if len(current) > 0 {
		parts = append(parts, string(current))
	}
	return parts
}

func utf16Length(body string) int { return len(utf16.Encode([]rune(body))) }

func encodeSubmit(destination, body string, encoding Encoding, udh []byte) (string, error) {
	address, err := encodeAddress(destination)
	if err != nil {
		return "", err
	}
	firstOctet := byte(0x01) // SMS-SUBMIT, no validity period.
	if len(udh) > 0 {
		firstOctet |= 0x40 // UDHI.
	}
	dcs := byte(0x00)
	var userData []byte
	var userDataLength int
	if encoding == EncodingGSM7 {
		dcs = 0x00
		septets := encodeGSM7(body)
		if len(udh) > 0 {
			userData = packUDHAndSeptets(udh, septets)
			userDataLength = (len(udh)*8 + len(septets)*7 + 6) / 7
		} else {
			userData = packSeptets(septets)
			userDataLength = len(septets)
		}
	} else {
		dcs = 0x08 // UCS-2.
		bodyBytes := utf16.Encode([]rune(body))
		encoded := make([]byte, 0, len(bodyBytes)*2)
		for _, value := range bodyBytes {
			encoded = append(encoded, byte(value>>8), byte(value))
		}
		if len(udh) > 0 {
			userData = append(append([]byte(nil), udh...), encoded...)
		} else {
			userData = encoded
		}
		userDataLength = len(userData)
	}
	pdu := []byte{0x00, firstOctet, 0x00}
	pdu = append(pdu, address...)
	pdu = append(pdu, 0x00, dcs, byte(userDataLength))
	pdu = append(pdu, userData...)
	return strings.ToUpper(hex.EncodeToString(pdu)), nil
}

func encodeAddress(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	ton := byte(0x81)
	if strings.HasPrefix(value, "+") {
		ton = 0x91
		value = value[1:]
	}
	if value == "" {
		return nil, fmt.Errorf("%w: empty destination", ErrInvalidPDU)
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return nil, fmt.Errorf("%w: invalid destination %q", ErrInvalidPDU, value)
		}
	}
	result := []byte{byte(len(value)), ton}
	for index := 0; index < len(value); index += 2 {
		low := value[index] - '0'
		high := byte(0x0f)
		if index+1 < len(value) {
			high = value[index+1] - '0'
		}
		result = append(result, low|(high<<4))
	}
	return result, nil
}

func encodeGSM7(body string) []byte {
	result := make([]byte, 0, GSM7Length(body))
	for _, value := range body {
		if code, ok := gsm7Reverse[value]; ok {
			result = append(result, code)
		} else {
			result = append(result, 0x1b, gsm7Extension[value])
		}
	}
	return result
}

func packSeptets(septets []byte) []byte {
	return packBits(nil, septets, 0)
}

func packUDHAndSeptets(udh, septets []byte) []byte {
	return packBits(udh, septets, len(udh)*8)
}

func packBits(prefix, septets []byte, offset int) []byte {
	result := append([]byte(nil), prefix...)
	for index, septet := range septets {
		bitPosition := offset + index*7
		bytePosition := bitPosition / 8
		bitOffset := bitPosition % 8
		for bit := 0; bit < 7; bit++ {
			if septet&(1<<bit) == 0 {
				continue
			}
			position := bytePosition + (bitOffset+bit)/8
			for len(result) <= position {
				result = append(result, 0)
			}
			result[position] |= 1 << ((bitOffset + bit) % 8)
		}
	}
	return result
}

func unpackSeptets(data []byte, count, offset int) []byte {
	result := make([]byte, 0, count)
	for index := 0; index < count; index++ {
		bitPosition := offset + index*7
		value := byte(0)
		for bit := 0; bit < 7; bit++ {
			position := bitPosition + bit
			if position/8 < len(data) && data[position/8]&(1<<uint(position%8)) != 0 {
				value |= 1 << uint(bit)
			}
		}
		result = append(result, value)
	}
	return result
}

func decodeGSM7(septets []byte) string {
	var result strings.Builder
	for index := 0; index < len(septets); index++ {
		value := septets[index]
		if value == 0x1b && index+1 < len(septets) {
			index++
			for character, code := range gsm7Extension {
				if code == septets[index] {
					result.WriteRune(character)
					break
				}
			}
			continue
		}
		if int(value) < len(gsm7Alphabet) {
			result.WriteRune(gsm7Alphabet[value])
		}
	}
	return result.String()
}

func decodeAddress(data []byte, offset *int) (string, error) {
	if *offset+2 > len(data) {
		return "", ErrInvalidPDU
	}
	digitCount := int(data[*offset])
	typeOfNumber := data[*offset+1]
	*offset += 2
	byteCount := (digitCount + 1) / 2
	if *offset+byteCount > len(data) {
		return "", ErrInvalidPDU
	}
	var result strings.Builder
	if typeOfNumber&0x70 == 0x10 {
		result.WriteByte('+')
	}
	for index := 0; index < byteCount; index++ {
		value := data[*offset+index]
		result.WriteByte('0' + value&0x0f)
		if len(result.String())-boolToInt(typeOfNumber&0x70 == 0x10) < digitCount && value>>4 != 0x0f {
			result.WriteByte('0' + value>>4)
		}
	}
	*offset += byteCount
	return result.String(), nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// DecodePDU decodes SMS-DELIVER and SMS-SUBMIT user data. It intentionally
// leaves timestamp formatting as the modem-provided semi-octet string; the
// database layer can convert it without losing the original value.
func DecodePDU(pdu string) (DecodedMessage, error) {
	data, err := hex.DecodeString(strings.TrimSpace(pdu))
	if err != nil {
		return DecodedMessage{}, fmt.Errorf("%w: hex: %v", ErrInvalidPDU, err)
	}
	if len(data) < 2 {
		return DecodedMessage{}, ErrInvalidPDU
	}
	offset := 0
	smscLength := int(data[offset])
	offset++
	if offset+smscLength > len(data) {
		return DecodedMessage{}, ErrInvalidPDU
	}
	offset += smscLength
	if offset >= len(data) {
		return DecodedMessage{}, ErrInvalidPDU
	}
	firstOctet := data[offset]
	offset++
	message := DecodedMessage{}
	if firstOctet&0x03 == 0x01 { // SMS-SUBMIT: TP-MR follows.
		if offset >= len(data) {
			return DecodedMessage{}, ErrInvalidPDU
		}
		offset++
	}
	sender, err := decodeAddress(data, &offset)
	if err != nil {
		return DecodedMessage{}, err
	}
	message.Sender = sender
	if offset+2 > len(data) {
		return DecodedMessage{}, ErrInvalidPDU
	}
	offset++ // PID
	dcs := data[offset]
	offset++
	if firstOctet&0x03 != 0x01 {
		if offset+7 > len(data) {
			return DecodedMessage{}, ErrInvalidPDU
		}
		message.ServiceTime = hex.EncodeToString(data[offset : offset+7])
		offset += 7
	}
	if offset >= len(data) {
		return DecodedMessage{}, ErrInvalidPDU
	}
	userDataLength := int(data[offset])
	offset++
	if offset > len(data) {
		return DecodedMessage{}, ErrInvalidPDU
	}
	userData := data[offset:]
	udhLength := 0
	if firstOctet&0x40 != 0 {
		if len(userData) == 0 {
			return DecodedMessage{}, ErrInvalidPDU
		}
		udhLength = int(userData[0]) + 1
		if udhLength > len(userData) {
			return DecodedMessage{}, ErrInvalidPDU
		}
		parseConcat(userData[1:udhLength], &message)
	}
	if dcs&0x0c == 0x08 {
		message.Encoding = EncodingUCS2
		payload := userData[udhLength:]
		if len(payload)%2 != 0 {
			return DecodedMessage{}, ErrInvalidPDU
		}
		units := make([]uint16, len(payload)/2)
		for index := range units {
			units[index] = uint16(payload[index*2])<<8 | uint16(payload[index*2+1])
		}
		message.Body = string(utf16.Decode(units))
	} else {
		message.Encoding = EncodingGSM7
		count := userDataLength
		if firstOctet&0x40 != 0 {
			count -= (udhLength*8 + 6) / 7
		}
		message.Body = decodeGSM7(unpackSeptets(userData, count, udhLength*8))
	}
	return message, nil
}

func parseConcat(udh []byte, message *DecodedMessage) {
	for index := 0; index+1 < len(udh); {
		length := int(udh[index+1])
		if index+2+length > len(udh) {
			return
		}
		if udh[index] == 0x00 && length == 3 {
			message.ConcatRef = udh[index+2]
			message.TotalParts = int(udh[index+3])
			message.PartNo = int(udh[index+4])
			return
		}
		index += 2 + length
	}
}
