package control

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

// buildGSMSMSSubmitPDU builds a single-segment GSM SMS-SUBMIT PDU without an
// SMSC address. UCS-2 is used deliberately so Chinese text and emoji are not
// silently corrupted. SendSMS remains behind an explicit verified-SMS runtime
// flag until the QDC507 WMS HIL has proved the complete request.
func buildGSMSMSSubmitPDU(number, body string) (string, error) {
	number = strings.TrimSpace(number)
	international := strings.HasPrefix(number, "+")
	if international {
		number = number[1:]
	}
	if !phoneNumberPattern.MatchString(number) {
		return "", errors.New("SMS target must contain 3 to 20 digits")
	}
	if strings.TrimSpace(body) == "" {
		return "", errors.New("SMS body must not be empty")
	}
	utf16Body := utf16.Encode([]rune(body))
	if len(utf16Body) > 70 {
		return "", errors.New("SMS body exceeds one UCS-2 segment")
	}

	address, err := encodeSemiOctets(number)
	if err != nil {
		return "", err
	}
	pdu := []byte{0x00, 0x01, 0x00, byte(len(number))}
	if international {
		pdu = append(pdu, 0x91)
	} else {
		pdu = append(pdu, 0x81)
	}
	pdu = append(pdu, address...)
	pdu = append(pdu, 0x00, 0x08, byte(len(utf16Body)*2))
	for _, value := range utf16Body {
		pdu = append(pdu, byte(value>>8), byte(value))
	}
	return strings.ToUpper(hex.EncodeToString(pdu)), nil
}

func encodeSemiOctets(number string) ([]byte, error) {
	if !phoneNumberPattern.MatchString(number) {
		return nil, fmt.Errorf("invalid phone number %q", number)
	}
	if len(number)%2 != 0 {
		number += "F"
	}
	result := make([]byte, 0, len(number)/2)
	for index := 0; index < len(number); index += 2 {
		high, err := hexDigit(number[index+1])
		if err != nil {
			return nil, err
		}
		low, err := hexDigit(number[index])
		if err != nil {
			return nil, err
		}
		result = append(result, high<<4|low)
	}
	return result, nil
}

func hexDigit(value byte) (byte, error) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', nil
	case value == 'F':
		return 0x0f, nil
	default:
		return 0, fmt.Errorf("invalid semi-octet digit %q", value)
	}
}
