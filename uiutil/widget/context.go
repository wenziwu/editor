package widget

import (
	"image/draw"

	"golang.org/x/image/font"
)

type Context interface {
	Image() draw.Image
	FontFace1() font.Face
}