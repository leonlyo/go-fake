package faketrace

import (
	"context"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"bou.ke/monkey"
)

const (
	writeCache = 1 << 20
	batchSize  = 1 << 12
)

type allocTag struct {
	t    int64
	p    uintptr
	size uintptr
	typ  *_type
	free bool
	// idx  uint32
}

func (a *allocTag) print() {
	if a.free {
		print(a.t, "_f(", toHex(uint64(a.p)), ", ", toHex(uint64(a.size)), ")\n")
	} else {
		if a.typ == nil {
			print(a.t, "_a(", toHex(uint64(a.p)), ", ", toHex(uint64(a.size)), ")\n")
		} else {
			print(a.t, "_a(", toHex(uint64(a.p)), ", ", toHex(uint64(a.size)), ", ", typeString(a.typ), ")\n")
		}
	}
}

func (a *allocTag) bytesCopy(out []byte) int {
	var (
		bl = 0
		bc = cap(out)
	)
	check := func() bool {
		return bl == bc
	}
	//时间戳
	bl += toDecimalCopy(out[bl:], uint64(a.t))
	if check() {
		return 0
	}
	//tag
	if a.free {
		bl += copy(out[bl:], "_f(")
	} else {
		bl += copy(out[bl:], "_a(")
	}
	if check() {
		return 0
	}
	//地址
	bl += toHexCopy(out[bl:], uint64(a.p))
	if check() {
		return 0
	}
	//分割逗号
	out[bl] = ','
	bl++
	if check() {
		return 0
	}
	//大小
	bl += toHexCopy(out[bl:], uint64(a.size))
	if check() {
		return 0
	}
	if a.typ != nil {
		//分割逗号
		out[bl] = ','
		bl++
		if check() {
			return 0
		}
		bl += copy(out[bl:], typeString(a.typ))
		if check() {
			return 0
		}
	}
	// out[bl] = ','
	// bl++
	// if check() {
	// 	return 0
	// }
	// bl += toDecimalCopy(out[bl:], uint64(a.idx))
	// if check() {
	// 	return 0
	// }
	out[bl] = ')'
	bl++
	if check() {
		return 0
	}
	out[bl] = '\n'
	bl++
	return bl
}

type _type struct{}

//go:linkname typeString runtime.(*_type).string
func typeString(t *_type) string

//go:linkname tracealloc runtime.tracealloc
func tracealloc(p unsafe.Pointer, size uintptr, typ *_type)

//go:linkname tracefree runtime.tracefree
func tracefree(p unsafe.Pointer, size uintptr)

//go:linkname tracegc runtime.tracegc
func tracegc()

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

func toHexCopy(out []byte, v uint64) int {
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
	return copy(out, buf[i:])
}

// func toDecimal(v uint64) string {
// 	const dig = "0123456789"
// 	var buf [100]byte
// 	i := len(buf)
// 	for i--; i > 0; i-- {
// 		buf[i] = dig[v%10]
// 		if v < 10 {
// 			break
// 		}
// 		v /= 10
// 	}
// 	return string(buf[i:])
// }

func toDecimalCopy(out []byte, v uint64) int {
	const dig = "0123456789"
	var buf [100]byte
	i := len(buf)
	for i--; i > 0; i-- {
		buf[i] = dig[v%10]
		if v < 10 {
			break
		}
		v /= 10
	}
	return copy(out, buf[i:])
}

//go:linkname debugvar runtime.debug
var debugvar struct{}

func parseGoVersion() (major, minor, patch int) {
	ver := runtime.Version()
	vars := strings.Split(ver[2:], ".")
	major, _ = strconv.Atoi(vars[0])
	minor, _ = strconv.Atoi(vars[1])
	patch, _ = strconv.Atoi(vars[2])
	return
}

func getOffset(minor int) int {
	switch minor {
	case 17:
		return 16
	default:
		return 17
	}
}

func fixDebugEnv() {
	_, minor, _ := parseGoVersion()
	off4byte := getOffset(minor)
	mallb := (*bool)(unsafe.Pointer(uintptr(unsafe.Pointer(&debugvar)) + 4*uintptr(off4byte-1)))
	*mallb = true
	allpc := (*int32)(unsafe.Pointer(uintptr(unsafe.Pointer(&debugvar)) + 4*uintptr(off4byte)))
	*allpc = 1
}

type FakeAlloctrace struct {
	allockPatch *monkey.PatchGuard
	freePatch   *monkey.PatchGuard
	gcPatch     *monkey.PatchGuard
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	writerN     int
	wbuf        [][]byte
	ring        *ringBuffer
}

func NewFakeAllocTrace(writerN, bufN int) *FakeAlloctrace {
	traceEnv := os.Getenv("alloctrace")
	if len(traceEnv) == 0 {
		return nil
	}
	if trace, err := strconv.Atoi(traceEnv); err != nil || trace == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &FakeAlloctrace{
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
		writerN: writerN,
		ring:    newRingBuffer(bufN),
	}
	f.init()
	go f.run()
	return f
}

func (f *FakeAlloctrace) init() {
	f.wbuf = make([][]byte, f.writerN)
	for i := 0; i < f.writerN; i++ {
		f.wbuf[i] = make([]byte, writeCache)
	}
	f.allockPatch = monkey.Patch(tracealloc, f.tracealloc)
	f.freePatch = monkey.Patch(tracefree, f.tracefree)
	f.gcPatch = monkey.Patch(tracegc, f.tracegc)
	fixDebugEnv()
}

func (f *FakeAlloctrace) Destory() {
	f.allockPatch.Unpatch()
	f.freePatch.Unpatch()
	f.gcPatch.Unpatch()
	f.cancel()
	<-f.done
}

func (f *FakeAlloctrace) tracealloc(p unsafe.Pointer, size uintptr, typ *_type) {
	tag := allocTag{
		t:    time.Now().UnixNano(),
		p:    uintptr(p),
		size: size,
		typ:  typ,
	}
	if f.ring.put(tag) {
		return
	}
	tag.print()
}

func (f *FakeAlloctrace) tracefree(p unsafe.Pointer, size uintptr) {
	tag := allocTag{
		t:    time.Now().UnixNano(),
		p:    uintptr(p),
		size: size,
		free: true,
	}
	if f.ring.put(tag) {
		return
	}
	tag.print()
}

func (f *FakeAlloctrace) tracegc() {

}

func printBuf(buf []byte) {
	var s string
	bp := (*reflect.SliceHeader)(unsafe.Pointer(&buf))
	hdr := (*reflect.StringHeader)(unsafe.Pointer(&s))
	hdr.Data = bp.Data
	hdr.Len = len(buf)
	print(s)
}

func (f *FakeAlloctrace) run() {
	if f.writerN < 1 {
		return
	}
	wg := sync.WaitGroup{}
	wg.Add(f.writerN)
	for i := 0; i < f.writerN; i++ {
		go func(idx int) {
			defer wg.Done()
			f.dump(f.ctx, f.wbuf[idx])
		}(i)
	}
	wg.Wait()
	println("---$$$$$$---", f.ring.empty())
	var (
		buf = f.wbuf[0]
		bl  = 0
	)
	for !f.ring.empty() {
		batch := f.ring.getOneBatch(false)
		println("---$$$$$$---count--", f.ring.CountIn(), "--", len(batch))
		for _, tag := range batch {
			if tag.p == 0 {
				continue
			}
			// tag.print()
			l := tag.bytesCopy(buf[bl:])
			if l == 0 {
				printBuf(buf[:bl])
				bl = 0
				l = tag.bytesCopy(buf[bl:])
			}
			bl += l
		}
	}
	printBuf(buf[:bl])
	println("---$$$$$$---end", f.ring.empty())
	close(f.done)
}
func (f *FakeAlloctrace) dump(ctx context.Context, buf []byte) {
	var (
		allocBuf [batchSize]allocTag
		bl       = 0
	)

	for {
		select {
		case <-ctx.Done():
			printBuf(buf[:bl])
			return
		default:
		}
		batch := f.ring.getOneBatch(true)
		if batch == nil {
			runtime.Gosched()
		} else {
			for total, n := 0, 0; total < len(batch); total += n {
				n = copy(allocBuf[:], batch[total:])
				for _, tag := range allocBuf[:n] {
					if tag.p == 0 {
						continue
					}
					l := tag.bytesCopy(buf[bl:])
					if l == 0 {
						printBuf(buf[:bl])
						bl = 0
						l = tag.bytesCopy(buf[bl:])
					}
					bl += l
				}
			}
		}
	}
}

type ringBuffer struct {
	r   uint32     //read cursor
	w   uint32     //write cursor
	b   int        //log_2 of the buf
	l   uint32     //buf size
	m   int        //index mask
	buf []allocTag //buf
	bc  []uint32   //batch count
	bb  int        //log_2 of the batch
	bm  int        //batch buf mask
}

func newRingBuffer(size int) *ringBuffer {
	b := loadFactor(size)
	bb := loadFactor(batchSize)
	size = 1 << b
	return &ringBuffer{
		b:   b,
		l:   uint32(size),
		m:   size - 1,
		buf: make([]allocTag, size),
		bb:  bb,
		bc:  make([]uint32, 1<<(b-bb)),
		bm:  1<<(b-bb) - 1,
	}
}

func loadFactor(count int) int {
	b := 0
	for (1 << b) < count {
		b++
	}
	return b
}

func (b *ringBuffer) put(in allocTag) bool {
	at := atomic.AddUint32(&b.w, 1)
	idx := b.bufIndex((at - 1))
	cIdx := b.batchCountIndex(idx)
	if at-atomic.LoadUint32(&b.r) > b.l {
		if !atomic.CompareAndSwapUint32(&b.w, at, at-1) {
			b.buf[idx].p = 0
			atomic.AddUint32(&b.bc[cIdx], 1)
		}
		return false
	}
	b.buf[idx] = in
	atomic.AddUint32(&b.bc[cIdx], 1)
	return true
}

func (b *ringBuffer) getOneBatch(hard bool) []allocTag {
	batch := uint32(1 << b.bb)
	w := atomic.LoadUint32(&b.w)
	r := atomic.LoadUint32(&b.r)
	diff := w - r
	if diff < batch {
		if hard {
			return nil
		}
		if diff == 0 {
			return nil
		}
		batch = diff
	}
	ridx := b.bufIndex(r)
	cIdx := b.batchCountIndex(ridx)
	if atomic.LoadUint32(&b.bc[cIdx]) < batch {
		return nil
	}
	if atomic.CompareAndSwapUint32(&b.r, r, r+batch) {
		atomic.StoreUint32(&b.bc[cIdx], 0)
		return b.buf[ridx : uint32(ridx)+batch]
	}
	return nil
}

func (b *ringBuffer) bufIndex(c uint32) int {
	return int(c) & b.m
}

func (b *ringBuffer) batchCountIndex(idx int) int {
	return (idx >> b.bb) & b.bm
}

func (b *ringBuffer) empty() bool {
	return b.r == b.w
}

func (b *ringBuffer) CountIn() int {
	return int(atomic.LoadUint32(&b.w) - atomic.LoadUint32(&b.r))
}
