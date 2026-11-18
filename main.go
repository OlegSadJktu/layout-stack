// layout-stack: MRU ("stack") keyboard layout switching for KDE on X11.
//
// Caps Lock (mapped to Menu via xkb option caps:menu) walks down the stack:
// first press -> 2nd layout, next press -> 3rd, and so on. The current layout
// moves to the top of the stack only once you actually type a character in it.
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

const (
	keysymMenu = 0xff67

	kbdIface = "org.kde.KeyboardLayouts"
)

type keyEvent struct {
	code  xproto.Keycode
	press bool
}

type keymap struct {
	keysym map[xproto.Keycode]uint32
	menu   map[xproto.Keycode]bool
	// Ctrl/Alt/Super/Hyper keys: a character typed with them held is a shortcut, not text.
	blocking map[xproto.Keycode]bool
}

func loadKeymap(c *xgb.Conn) (*keymap, error) {
	setup := xproto.Setup(c)
	minK, maxK := setup.MinKeycode, setup.MaxKeycode
	km, err := xproto.GetKeyboardMapping(c, minK, byte(maxK-minK+1)).Reply()
	if err != nil {
		return nil, err
	}
	m := &keymap{
		keysym:   map[xproto.Keycode]uint32{},
		menu:     map[xproto.Keycode]bool{},
		blocking: map[xproto.Keycode]bool{},
	}
	per := int(km.KeysymsPerKeycode)
	for i := 0; i <= int(maxK-minK); i++ {
		code := xproto.Keycode(int(minK) + i)
		ks := uint32(km.Keysyms[i*per])
		m.keysym[code] = ks
		if ks == keysymMenu {
			m.menu[code] = true
		}
	}
	mm, err := xproto.GetModifierMapping(c).Reply()
	if err != nil {
		return nil, err
	}
	per = int(mm.KeycodesPerModifier)
	// Modifier indices: 2 Control, 3 Mod1 (Alt), 5 Mod3 (Hyper), 6 Mod4 (Super).
	for _, mod := range []int{2, 3, 5, 6} {
		for _, code := range mm.Keycodes[mod*per : (mod+1)*per] {
			if code != 0 {
				m.blocking[code] = true
			}
		}
	}
	return m, nil
}

func printable(ks uint32) bool {
	switch {
	case ks == 0:
		return false
	case ks >= 0xfe00 && ks <= 0xffff: // function keys, modifiers, keypad, editing keys
		return false
	case ks >= 0x01000000: // Unicode keysyms are printable, vendor (XF86...) ones are not
		return ks <= 0x0110ffff
	}
	return true
}

// grabMenu grabs the Menu key(s) so they don't reach applications.
func grabMenu(c *xgb.Conn, root xproto.Window, km *keymap) error {
	xproto.UngrabKey(c, xproto.GrabAny, root, xproto.ModMaskAny)
	for code := range km.menu {
		err := xproto.GrabKeyChecked(c, false, root, xproto.ModMaskAny, code,
			xproto.GrabModeAsync, xproto.GrabModeAsync).Check()
		if err != nil {
			return fmt.Errorf("grab keycode %d (is Menu bound to a KDE shortcut?): %v", code, err)
		}
	}
	return nil
}

// --- KDE layouts over D-Bus ---

type layouts struct {
	obj dbus.BusObject
}

func (l layouts) list() ([]string, error) {
	var raw []struct{ Short, Display, Long string }
	if err := l.obj.Call(kbdIface+".getLayoutsList", 0).Store(&raw); err != nil {
		return nil, err
	}
	names := make([]string, len(raw))
	for i, r := range raw {
		names[i] = r.Short
	}
	return names, nil
}

func (l layouts) get() (uint32, error) {
	var cur uint32
	err := l.obj.Call(kbdIface+".getLayout", 0).Store(&cur)
	return cur, err
}

func (l layouts) set(i uint32) error {
	return l.obj.Call(kbdIface+".setLayout", 0, i).Err
}

// --- XRecord over a raw X connection ---
//
// xgb can't consume RecordEnableContext's stream of replies, so the recording
// connection speaks the few requests it needs directly.

func readXauth(display string) (name, data []byte) {
	path := os.Getenv("XAUTHORITY")
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".Xauthority")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer f.Close()
	readStr := func() ([]byte, error) {
		var n uint16
		if err := binary.Read(f, binary.BigEndian, &n); err != nil {
			return nil, err
		}
		b := make([]byte, n)
		_, err := io.ReadFull(f, b)
		return b, err
	}
	for {
		var family uint16
		if binary.Read(f, binary.BigEndian, &family) != nil {
			return nil, nil
		}
		var fields [4][]byte // address, number, name, data
		for i := range fields {
			if fields[i], err = readStr(); err != nil {
				return nil, nil
			}
		}
		if string(fields[2]) == "MIT-MAGIC-COOKIE-1" && (len(fields[1]) == 0 || string(fields[1]) == display) {
			return fields[2], fields[3]
		}
	}
}

func pad4(n int) int { return (4 - n%4) % 4 }

func recordKeys(opcode byte, out chan<- keyEvent) error {
	disp := os.Getenv("DISPLAY")
	if !strings.HasPrefix(disp, ":") {
		return fmt.Errorf("only local displays are supported, DISPLAY=%q", disp)
	}
	num, _, _ := strings.Cut(disp[1:], ".")
	conn, err := net.Dial("unix", "/tmp/.X11-unix/X"+num)
	if err != nil {
		return err
	}
	defer conn.Close()

	le := binary.LittleEndian
	authName, authData := readXauth(num)
	req := make([]byte, 12)
	req[0] = 'l'
	le.PutUint16(req[2:], 11)
	le.PutUint16(req[6:], uint16(len(authName)))
	le.PutUint16(req[8:], uint16(len(authData)))
	req = append(req, authName...)
	req = append(req, make([]byte, pad4(len(authName)))...)
	req = append(req, authData...)
	req = append(req, make([]byte, pad4(len(authData)))...)
	if _, err := conn.Write(req); err != nil {
		return err
	}
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return err
	}
	setup := make([]byte, int(le.Uint16(hdr[6:]))*4)
	if _, err := io.ReadFull(conn, setup); err != nil {
		return err
	}
	if hdr[0] != 1 {
		return fmt.Errorf("X connection refused: %s", setup)
	}
	ctx := le.Uint32(setup[4:]) | 1 // resource-id-base | first id

	var reqs []byte
	// RecordQueryVersion 1.13 (seq 1)
	reqs = le.AppendUint16(append(reqs, opcode, 0), 2)
	reqs = le.AppendUint16(le.AppendUint16(reqs, 1), 13)
	// RecordCreateContext (seq 2): all clients, device KeyPress..KeyRelease
	reqs = le.AppendUint16(append(reqs, opcode, 1), 12)
	reqs = le.AppendUint32(reqs, ctx)
	reqs = append(reqs, 0, 0, 0, 0) // element header + pad
	reqs = le.AppendUint32(reqs, 1) // client specs
	reqs = le.AppendUint32(reqs, 1) // ranges
	reqs = le.AppendUint32(reqs, 3) // XRecordAllClients
	rng := make([]byte, 24)
	rng[18], rng[19] = xproto.KeyPress, xproto.KeyRelease
	reqs = append(reqs, rng...)
	// RecordEnableContext (seq 3)
	reqs = le.AppendUint16(append(reqs, opcode, 5), 2)
	reqs = le.AppendUint32(reqs, ctx)
	if _, err := conn.Write(reqs); err != nil {
		return err
	}

	buf := make([]byte, 32)
	for {
		if _, err := io.ReadFull(conn, buf); err != nil {
			return err
		}
		switch buf[0] {
		case 0:
			return fmt.Errorf("X error %d on RECORD request seq %d", buf[1], le.Uint16(buf[2:]))
		case 1:
		default:
			continue
		}
		data := make([]byte, int(le.Uint32(buf[4:]))*4)
		if _, err := io.ReadFull(conn, data); err != nil {
			return err
		}
		if le.Uint16(buf[2:]) != 3 || buf[1] != 0 { // only "from server" data of EnableContext
			continue
		}
		for len(data) >= 32 {
			typ := data[0] & 0x7f
			if typ == xproto.KeyPress || typ == xproto.KeyRelease {
				out <- keyEvent{code: xproto.Keycode(data[1]), press: typ == xproto.KeyPress}
			}
			data = data[32:]
		}
	}
}

// --- stack ---

type state struct {
	names   []string
	stack   []uint32
	current uint32
	// when we last asked KDE for each layout: KDE echoes layoutChanged
	// (twice) for our own switches, and those must not be taken as external
	ours map[uint32]time.Time
	held map[xproto.Keycode]bool
}

// reset syncs with KDE's layout list. KDE also emits layoutListChanged when
// nothing changed, so the stack is kept as is if the list is the same.
func (s *state) reset(names []string, cur uint32) {
	if !slices.Equal(names, s.names) {
		// indices now mean different layouts: start over
		s.stack = []uint32{cur}
	}
	s.names, s.current = names, cur
	for i := range names {
		if !contains(s.stack, uint32(i)) {
			s.stack = append(s.stack, uint32(i))
		}
	}
	s.logStack()
}

func contains(s []uint32, v uint32) bool { return index(s, v) >= 0 }

func index(s []uint32, v uint32) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func (s *state) logStack() {
	parts := make([]string, len(s.stack))
	for i, l := range s.stack {
		parts[i] = s.names[l]
	}
	log.Printf("stack: %s", strings.Join(parts, " "))
}

func (s *state) promote() {
	i := index(s.stack, s.current)
	if i <= 0 {
		return
	}
	copy(s.stack[1:i+1], s.stack[:i])
	s.stack[0] = s.current
	s.logStack()
}

func (s *state) next() uint32 {
	i := index(s.stack, s.current)
	return s.stack[(i+1)%len(s.stack)]
}

func main() {
	log.SetFlags(0)

	xc, err := xgb.NewConn()
	if err != nil {
		log.Fatalf("X: %v", err)
	}
	root := xproto.Setup(xc).DefaultScreen(xc).Root
	km, err := loadKeymap(xc)
	if err != nil {
		log.Fatalf("keymap: %v", err)
	}
	if len(km.menu) == 0 {
		log.Fatalf("no key produces Menu; set xkb option caps:menu")
	}
	if err := grabMenu(xc, root, km); err != nil {
		log.Fatal(err)
	}
	ext, err := xproto.QueryExtension(xc, uint16(len("RECORD")), "RECORD").Reply()
	if err != nil || !ext.Present {
		log.Fatalf("X RECORD extension not available")
	}

	bus, err := dbus.ConnectSessionBus()
	if err != nil {
		log.Fatalf("D-Bus: %v", err)
	}
	kl := layouts{bus.Object("org.kde.keyboard", "/Layouts")}
	if err := bus.AddMatchSignal(dbus.WithMatchObjectPath("/Layouts"), dbus.WithMatchInterface(kbdIface)); err != nil {
		log.Fatalf("D-Bus match: %v", err)
	}
	sigs := make(chan *dbus.Signal, 32)
	bus.Signal(sigs)

	s := &state{ours: map[uint32]time.Time{}, held: map[xproto.Keycode]bool{}}
	reload := func() {
		names, err := kl.list()
		if err != nil {
			log.Fatalf("getLayoutsList: %v", err)
		}
		cur, err := kl.get()
		if err != nil {
			log.Fatalf("getLayout: %v", err)
		}
		s.reset(names, cur)
	}
	reload()

	keys := make(chan keyEvent, 256)
	go func() {
		log.Fatalf("RECORD: %v", recordKeys(ext.MajorOpcode, keys))
	}()

	mapping := make(chan struct{}, 1)
	go func() {
		for {
			ev, err := xc.WaitForEvent()
			if ev == nil && err == nil {
				log.Fatal("X connection closed")
			}
			if _, ok := ev.(xproto.MappingNotifyEvent); ok {
				select {
				case mapping <- struct{}{}:
				default:
				}
			}
		}
	}()

	for {
		select {
		case k := <-keys:
			switch {
			case km.menu[k.code]:
				if !k.press || len(s.stack) < 2 {
					break
				}
				n := s.next()
				if err := kl.set(n); err != nil {
					log.Printf("setLayout: %v", err)
					break
				}
				s.current = n
				s.ours[n] = time.Now()
			case km.blocking[k.code]:
				if k.press {
					s.held[k.code] = true
				} else {
					delete(s.held, k.code)
				}
			case k.press && len(s.held) == 0 && printable(km.keysym[k.code]):
				s.promote()
			}

		case sig := <-sigs:
			switch sig.Name {
			case kbdIface + ".layoutChanged":
				idx, ok := sig.Body[0].(uint32)
				if !ok {
					break
				}
				if t, ok := s.ours[idx]; ok && time.Since(t) < time.Second {
					break
				}
				// changed by something else (Meta+Shift+K, tray, ...)
				s.current = idx
			case kbdIface + ".layoutListChanged":
				reload()
			}

		case <-mapping:
			nkm, err := loadKeymap(xc)
			if err != nil {
				log.Printf("keymap reload: %v", err)
				break
			}
			km = nkm
			s.held = map[xproto.Keycode]bool{}
			if err := grabMenu(xc, root, km); err != nil {
				log.Print(err)
			}
		}
	}
}
