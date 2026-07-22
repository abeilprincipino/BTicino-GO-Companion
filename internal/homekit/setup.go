package homekit

import (
	"bticino-go-companion/internal/config"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/brutella/hap/accessory"
	qrcode "github.com/yeqown/go-qrcode/v2"
	"github.com/yeqown/go-qrcode/writer/standard"
)

const homeKitSetupIDLength = 4

func SetupURI(homeKit config.HomeKit) (string, error) {
	pin, err := strconv.ParseUint(strings.ReplaceAll(homeKit.PIN, "-", ""), 10, 32)
	if err != nil {
		return "", fmt.Errorf("parse homekit setup pin: %w", err)
	}
	setupID := strings.ToUpper(strings.TrimSpace(homeKit.SetupID))
	if len(setupID) != homeKitSetupIDLength {
		return "", errors.New("homekit setup id must contain four characters")
	}

	payload := uint64(accessory.TypeBridge)<<31 | 2<<27 | pin
	encoded := strings.ToUpper(strconv.FormatUint(payload, 36))
	return "X-HM://" + strings.Repeat("0", max(0, 9-len(encoded))) + encoded + setupID, nil
}

func SetupQRCodePNG(homeKit config.HomeKit) ([]byte, error) {
	setupURI, err := SetupURI(homeKit)
	if err != nil {
		return nil, err
	}

	code, err := qrcode.New(setupURI)
	if err != nil {
		return nil, fmt.Errorf("create homekit qr code: %w", err)
	}

	var buffer bytes.Buffer
	writer := standard.NewWithWriter(nopWriteCloser{Writer: &buffer}, standard.WithBuiltinImageEncoder(standard.PNG_FORMAT), standard.WithQRWidth(8))
	if err := code.Save(writer); err != nil {
		return nil, fmt.Errorf("encode homekit qr code: %w", err)
	}

	return buffer.Bytes(), nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
