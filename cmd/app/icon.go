//go:build windows

package main

import "encoding/binary"

// trayIconICO builds a minimal 16x16 32bpp .ico in memory: two-tone
// square (red RU half → blue EN half) so no icon file needs shipping.
func trayIconICO() []byte {
	const w, h = 16, 16

	xor := make([]byte, w*h*4) // BGRA, bottom-up
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			if x < w/2 {
				// left: warm red (RU)
				xor[i+0], xor[i+1], xor[i+2], xor[i+3] = 0x40, 0x40, 0xC8, 0xFF
			} else {
				// right: cool blue (EN)
				xor[i+0], xor[i+1], xor[i+2], xor[i+3] = 0xD0, 0x78, 0x30, 0xFF
			}
		}
	}
	// Thin white arrow ">" to hint at RU→EN flow.
	for x := 6; x <= 10; x++ {
		for _, y := range []int{8 - (x - 6), 7 + (x - 6)} {
			if y >= 0 && y < h {
				i := (y*w + x) * 4
				xor[i+0], xor[i+1], xor[i+2], xor[i+3] = 0xFF, 0xFF, 0xFF, 0xFF
			}
		}
	}

	andMask := make([]byte, h*4) // 1bpp, all zero = fully opaque

	img := make([]byte, 40+len(xor)+len(andMask))
	binary.LittleEndian.PutUint32(img[0:], 40)  // BITMAPINFOHEADER size
	binary.LittleEndian.PutUint32(img[4:], w)   // width
	binary.LittleEndian.PutUint32(img[8:], h*2) // height (XOR+AND)
	binary.LittleEndian.PutUint16(img[12:], 1)  // planes
	binary.LittleEndian.PutUint16(img[14:], 32) // bpp
	binary.LittleEndian.PutUint32(img[20:], uint32(len(xor)+len(andMask)))
	copy(img[40:], xor)
	copy(img[40+len(xor):], andMask)

	ico := make([]byte, 6+16)
	// ICONDIR
	binary.LittleEndian.PutUint16(ico[2:], 1) // type: icon
	binary.LittleEndian.PutUint16(ico[4:], 1) // count
	// ICONDIRENTRY
	ico[6] = w                                  // width
	ico[7] = h                                  // height
	binary.LittleEndian.PutUint16(ico[10:], 1)  // planes
	binary.LittleEndian.PutUint16(ico[12:], 32) // bpp
	binary.LittleEndian.PutUint32(ico[14:], uint32(len(img)))
	binary.LittleEndian.PutUint32(ico[18:], 6+16)
	return append(ico, img...)
}
