package workload

import (
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
)

const Kernel = "mandelbrot.v1"

type Input struct {
	Kernel   string  `json:"kernel"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	MaxIter  int     `json:"max_iter"`
	XMin     float64 `json:"xmin"`
	XMax     float64 `json:"xmax"`
	YMin     float64 `json:"ymin"`
	YMax     float64 `json:"ymax"`
	TileSize int     `json:"tile_size"`
}

type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

type Params struct {
	Input
	Rect
}

type Tile struct {
	Rect
	Data []byte `json:"data_b64"`
}

func (in Input) Validate() error {
	if in.Kernel != Kernel || in.Width < 1 || in.Width > 2048 || in.Height < 1 || in.Height > 2048 ||
		in.MaxIter < 1 || in.MaxIter > 2048 || in.TileSize < 16 || in.TileSize > 64 {
		return errors.New("unsupported kernel, dimensions, iterations or tile size")
	}
	dx, dy := in.XMax-in.XMin, in.YMax-in.YMin
	// Multiplication precedes division in the pixel mapping.
	if !(dx > 0 && dy > 0) || math.IsInf(dx*float64(in.Width), 0) || math.IsInf(dy*float64(in.Height), 0) {
		return errors.New("coordinate ranges must be finite and ordered")
	}
	return nil
}

func (in Input) Partition() []Rect {
	var tiles []Rect
	for y := 0; y < in.Height; y += in.TileSize {
		for x := 0; x < in.Width; x += in.TileSize {
			tiles = append(tiles, Rect{x, y, min(in.TileSize, in.Width-x), min(in.TileSize, in.Height-y)})
		}
	}
	return tiles
}

func Reference(p Params) []byte {
	data := make([]byte, 2*p.W*p.H)
	for row := range p.H {
		for col := range p.W {
			x := p.XMin + float64((float64(p.X+col)+0.5)*(p.XMax-p.XMin))/float64(p.Width)
			y := p.YMax - float64((float64(p.Y+row)+0.5)*(p.YMax-p.YMin))/float64(p.Height)
			var zr, zi float64
			var n uint16
			for int(n) < p.MaxIter {
				r2, i2 := float64(zr*zr), float64(zi*zi)
				if !(r2+i2 <= 4) {
					break
				}
				zr, zi = r2-i2+x, float64(float64(2*zr)*zi)+y
				n++
			}
			binary.LittleEndian.PutUint16(data[2*(row*p.W+col):], n)
		}
	}
	return data
}

func ValidTile(r Rect, data []byte, limit int) bool {
	if len(data) != 2*r.W*r.H {
		return false
	}
	for i := 0; i < len(data); i += 2 {
		if int(binary.LittleEndian.Uint16(data[i:])) > limit {
			return false
		}
	}
	return true
}

func PNG(out io.Writer, in Input, tiles []Tile) error {
	img := image.NewRGBA(image.Rect(0, 0, in.Width, in.Height))
	for _, t := range tiles {
		for y := range t.H {
			for x := range t.W {
				n := int(binary.LittleEndian.Uint16(t.Data[2*(y*t.W+x):]))
				c := color.RGBA{A: 255}
				if n != in.MaxIter {
					c.R, c.G, c.B = byte(9*n), byte(5*n), byte(13*n)
				}
				img.SetRGBA(t.X+x, t.Y+y, c)
			}
		}
	}
	return png.Encode(out, img)
}
