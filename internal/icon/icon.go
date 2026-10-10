// Package icon 在运行时绘制程序图标（无需二进制素材）：蓝色圆角方块 + 白色云朵，可带状态圆点。
package icon

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
)

// State 是托盘图标状态。
type State int

const (
	Idle    State = iota // 未挂载：无圆点
	Running              // 已挂载：绿色圆点
	Busy                 // 处理中：橙色圆点
	Error                // 出错：红色圆点
)

var (
	bgTop    = color.NRGBA{0x3b, 0x82, 0xf6, 0xff}
	bgBottom = color.NRGBA{0x1d, 0x4e, 0xd8, 0xff}
	white    = color.NRGBA{0xff, 0xff, 0xff, 0xff}
	dots     = map[State]color.NRGBA{
		Running: {0x22, 0xc5, 0x5e, 0xff},
		Busy:    {0xf5, 0x9e, 0x0b, 0xff},
		Error:   {0xef, 0x44, 0x44, 0xff},
	}
)

func roundedSquare(x, y float64) bool {
	const m, r = 0.04, 0.22
	if x < m || x > 1-m || y < m || y > 1-m {
		return false
	}
	cx := math.Max(m+r-x, x-(1-m-r))
	cy := math.Max(m+r-y, y-(1-m-r))
	if cx > 0 && cy > 0 {
		return cx*cx+cy*cy <= r*r
	}
	return true
}

func circle(x, y, cx, cy, r float64) bool { return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r }

func cloud(x, y float64) bool {
	if circle(x, y, 0.36, 0.56, 0.15) || circle(x, y, 0.53, 0.46, 0.20) || circle(x, y, 0.68, 0.58, 0.13) {
		return true
	}
	// 底部胶囊形
	const y0, h, x0, x1 = 0.62, 0.09, 0.32, 0.70
	if x >= x0 && x <= x1 && math.Abs(y-y0) <= h {
		return true
	}
	return circle(x, y, x0, y0, h) || circle(x, y, x1, y0, h)
}

// sample 返回单位坐标处的颜色。
func sample(x, y float64, st State) color.NRGBA {
	dot, hasDot := dots[st]
	if hasDot {
		if circle(x, y, 0.78, 0.78, 0.17) {
			return dot
		}
		if circle(x, y, 0.78, 0.78, 0.23) {
			return white
		}
	}
	if !roundedSquare(x, y) {
		return color.NRGBA{}
	}
	if cloud(x, y) {
		return white
	}
	t := y
	lerp := func(a, b uint8) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*t) }
	return color.NRGBA{lerp(bgTop.R, bgBottom.R), lerp(bgTop.G, bgBottom.G), lerp(bgTop.B, bgBottom.B), 0xff}
}

// Image 以 4×4 超采样抗锯齿绘制 size×size 的图标。
func Image(size int, st State) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					c := sample((float64(px)+(float64(sx)+0.5)/ss)/float64(size), (float64(py)+(float64(sy)+0.5)/ss)/float64(size), st)
					fa := float64(c.A) / 255
					r += float64(c.R) * fa
					g += float64(c.G) * fa
					b += float64(c.B) * fa
					a += fa
				}
			}
			if a == 0 {
				continue
			}
			img.SetNRGBA(px, py, color.NRGBA{uint8(r / a), uint8(g / a), uint8(b / a), uint8(a / (ss * ss) * 255)})
		}
	}
	return img
}

// PNG 返回 PNG 编码的图标。
func PNG(size int, st State) []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, Image(size, st))
	return buf.Bytes()
}

// dib 把图像编码为 ICO 中使用的 32 位 BMP（含 AND 掩码）。
func dib(img *image.NRGBA) []byte {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	maskStride := ((w + 31) / 32) * 4
	var buf bytes.Buffer
	hdr := struct {
		Size                     uint32
		Width, Height            int32
		Planes, BitCount         uint16
		Compression, SizeImage   uint32
		XPPM, YPPM, Used, Import uint32
	}{40, int32(w), int32(h * 2), 1, 32, 0, uint32(w*h*4 + maskStride*h), 0, 0, 0, 0}
	_ = binary.Write(&buf, binary.LittleEndian, hdr)
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			c := img.NRGBAAt(x, y)
			buf.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}
	mask := make([]byte, maskStride)
	for y := h - 1; y >= 0; y-- {
		for i := range mask {
			mask[i] = 0
		}
		for x := 0; x < w; x++ {
			if img.NRGBAAt(x, y).A == 0 {
				mask[x/8] |= 0x80 >> (x % 8)
			}
		}
		buf.Write(mask)
	}
	return buf.Bytes()
}

// ICO 返回包含多个尺寸的 .ico 数据（256 使用 PNG 压缩，其余为 BMP 以兼容所有 Windows 接口）。
func ICO(st State, sizes ...int) []byte {
	if len(sizes) == 0 {
		sizes = []int{16, 20, 24, 32, 48}
	}
	blobs := make([][]byte, len(sizes))
	for i, s := range sizes {
		if s >= 256 {
			blobs[i] = PNG(s, st)
		} else {
			blobs[i] = dib(Image(s, st))
		}
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, [3]uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for i, s := range sizes {
		dim := byte(s)
		if s >= 256 {
			dim = 0
		}
		buf.Write([]byte{dim, dim, 0, 0})
		_ = binary.Write(&buf, binary.LittleEndian, [2]uint16{1, 32})
		_ = binary.Write(&buf, binary.LittleEndian, [2]uint32{uint32(len(blobs[i])), uint32(offset)})
		offset += len(blobs[i])
	}
	for _, b := range blobs {
		buf.Write(b)
	}
	return buf.Bytes()
}
