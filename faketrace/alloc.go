package faketrace

import (
	"unsafe"

	"bou.ke/monkey"
)

//go:linkname _type runtime._type
type _type struct{}

//go:linkname typeString runtime.(*_type).string
func typeString(t *_type) string

//go:linkname tracealloc runtime.tracealloc
func tracealloc(p unsafe.Pointer, size uintptr, typ *_type)

func tracealloc2(p unsafe.Pointer, size uintptr, typ *_type) {
	if typ == nil {
		print("tracealloc(", p, ", ", toHex(uint64(size)), ")\n")
	} else {
		print("tracealloc(", p, ", ", toHex(uint64(size)), ", ", typeString(typ), ")\n")
	}
}

//go:linkname tracefree runtime.tracefree
func tracefree(p unsafe.Pointer, size uintptr)
func tracefree2(p unsafe.Pointer, size uintptr) {
	print("tracefree(", p, ", ", toHex(uint64(size)), ")\n")
}

func toHex(v uint64) string {
	const dig = "0123456789abcdef"
	var buf [100]byte
	i := len(buf)
	for i--; i > 0; i-- {
		buf[i] = dig[v%16]
		if v < 16 {
			break
		}
		v /= 16
	}
	i--
	buf[i] = 'x'
	i--
	buf[i] = '0'
	return string(buf[i:])
}

type fakealloctrace struct {
	allockPatch *monkey.PatchGuard
	freePatch   *monkey.PatchGuard
}

func NewFakeAllocTrace() *fakealloctrace {
	return &fakealloctrace{
		allockPatch: monkey.Patch(tracealloc, tracealloc2),
		freePatch:   monkey.Patch(tracefree, tracefree2),
	}
}

func (f *fakealloctrace) Destory() {
	f.allockPatch.Unpatch()
	f.freePatch.Unpatch()
}
