package codec

import (
	"atlas-render/internal/model"
	"bytes"
	"image"
	"image/png"
)

func Decode(data []byte) (image.Image, error) {
	if len(data) == 0 || len(data) > 2<<20 {
		return nil, model.Invalid("PNG size must be 1..2MiB")
	}
	cfg, e := png.DecodeConfig(bytes.NewReader(data))
	if e != nil {
		return nil, model.Invalid("invalid PNG: %v", e)
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 1024 || cfg.Height > 1024 {
		return nil, model.Invalid("asset dimensions must be 1..1024")
	}
	img, e := png.Decode(bytes.NewReader(data))
	if e != nil {
		return nil, model.Invalid("PNG decoding failed: %v", e)
	}
	return img, nil
}
func Encode(img image.Image) ([]byte, error) {
	var b bytes.Buffer
	e := png.Encode(&b, img)
	return b.Bytes(), e
}

func DecodeOutput(data []byte) (image.Image, error) {
	if len(data) > 40<<20 {
		return nil, model.Invalid("output PNG exceeds limit")
	}
	cfg, e := png.DecodeConfig(bytes.NewReader(data))
	if e != nil {
		return nil, e
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 3072 || cfg.Height > 3072 {
		return nil, model.Invalid("invalid output dimensions")
	}
	return png.Decode(bytes.NewReader(data))
}
