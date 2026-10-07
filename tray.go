package main

// tray.go is the daemon's tray icon: a StatusNotifierItem spoken directly over
// D-Bus, with its menu over com.canonical.dbusmenu. There is no XEmbed
// fallback and no desktop-specific code. Ported from ts6tray: same protocol,
// same host quirks.
//
// The icon is a gauge (a circle filled to the share of CPUs the mode allows),
// drawn in Go on every change and
// shipped as ARGB32 pixmaps at 22/32/48 px. IconName stays empty on purpose,
// because a host that sees a name prefers it and draws the theme's icon
// instead of ours (see export()).
//
// Everything unexported in this file is either a method or prefixed "tray",
// because the other files share this package.

import (
	"context"
	"fmt"
	"image/color"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

const (
	trayItemPath    = dbus.ObjectPath("/StatusNotifierItem")
	trayMenuPath    = dbus.ObjectPath("/MenuBar")
	trayItemIface   = "org.kde.StatusNotifierItem"
	trayMenuIface   = "com.canonical.dbusmenu"
	trayWatcherName = "org.kde.StatusNotifierWatcher"
	trayWatcherPath = dbus.ObjectPath("/StatusNotifierWatcher")
)

// trayBackend is what the tray needs from the daemon: *sliceBackend.
type trayBackend interface {
	Snapshot() sliceSnapshot
	SetMode(mode string) error
	Changed() <-chan struct{}
}

// trayModes is the cycle order of left-click and the order of the menu rows.
var trayModes = []string{"green", "yellow", "red"}

// trayNext is the mode after mode in the cycle; anything unknown (including
// "", before the first apply) goes to the first.
func trayNext(mode string) string {
	for i, m := range trayModes {
		if m == mode {
			return trayModes[(i+1)%len(trayModes)]
		}
	}
	return trayModes[0]
}

func trayModeName(mode string) string {
	return strings.ToUpper(mode[:1]) + mode[1:]
}

// --- pixmaps ---------------------------------------------------------------

// trayPixmap is one entry of the SNI IconPixmap array: width, height and
// width*height ARGB32 pixels, big-endian, not premultiplied. Signature (iiay).
type trayPixmap struct {
	Width  int32
	Height int32
	Data   []byte
}

// trayTooltip is the SNI ToolTip property: (sa(iiay)ss).
type trayTooltip struct {
	IconName    string
	IconPixmap  []trayPixmap
	Title       string
	Description string
}

// trayCanvas is a tiny premultiplied-alpha painter used to draw the icons.
// It works at trayOversample times the target size and box-filters down, which
// buys antialiasing for free without any drawing library.
type trayCanvas struct {
	n  int // side length in samples
	px []color.RGBA
}

const trayOversample = 4

func trayNewCanvas(n int) *trayCanvas {
	return &trayCanvas{n: n, px: make([]color.RGBA, n*n)}
}

// trayShape reports whether a point (in 0..1 icon space) is inside it.
type trayShape func(x, y float64) bool

// fill paints col (opaque) over every sample inside s.
func (cv *trayCanvas) fill(s trayShape, col color.RGBA) {
	for yi := 0; yi < cv.n; yi++ {
		y := (float64(yi) + 0.5) / float64(cv.n)
		for xi := 0; xi < cv.n; xi++ {
			if s((float64(xi)+0.5)/float64(cv.n), y) {
				cv.px[yi*cv.n+xi] = col
			}
		}
	}
}

// pixmap box-filters the canvas down to size px and encodes ARGB32 big-endian,
// un-premultiplied.
func (cv *trayCanvas) pixmap(size int) trayPixmap {
	step := cv.n / size
	out := make([]byte, size*size*4)
	area := float64(step * step)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, b, a float64
			for sy := 0; sy < step; sy++ {
				for sx := 0; sx < step; sx++ {
					p := cv.px[(y*step+sy)*cv.n+(x*step+sx)]
					r += float64(p.R)
					g += float64(p.G)
					b += float64(p.B)
					a += float64(p.A)
				}
			}
			r, g, b, a = r/area, g/area, b/area, a/area
			// Stored premultiplied; SNI wants straight ARGB32.
			if a > 0 {
				r = math.Min(255, r*255/a)
				g = math.Min(255, g*255/a)
				b = math.Min(255, b*255/a)
			}
			o := (y*size + x) * 4
			out[o+0] = byte(a + 0.5)
			out[o+1] = byte(r + 0.5)
			out[o+2] = byte(g + 0.5)
			out[o+3] = byte(b + 0.5)
		}
	}
	return trayPixmap{Width: int32(size), Height: int32(size), Data: out}
}

// trayDisc is a filled circle of radius r about the icon's centre.
func trayDisc(r float64) trayShape {
	return func(x, y float64) bool {
		dx, dy := x-0.5, y-0.5
		return dx*dx+dy*dy <= r*r
	}
}

// The icon is a gauge: a circle with a black outline whose lower part is
// filled with the mode colour like a liquid level, the rest light grey. The
// filled share of the area is the share of the CPUs Claude may use.
const (
	trayDotR      = 0.47 // outer radius, outline included
	trayDotStroke = 0.08 // outline and level line thickness (1.75 px at 22)
	trayDotInner  = trayDotR - trayDotStroke
)

var (
	trayGreen   = color.RGBA{0x00, 0xc0, 0x00, 0xff}
	trayYellow  = color.RGBA{0xff, 0xd0, 0x00, 0xff}
	trayRed     = color.RGBA{0xe0, 0x00, 0x00, 0xff}
	trayEmpty   = color.RGBA{0xc0, 0xc0, 0xc0, 0xff} // the unfilled part
	trayOutline = color.RGBA{0x00, 0x00, 0x00, 0xff}
)

// trayFillFor is the liquid colour of a mode.
func trayFillFor(mode string) color.RGBA {
	switch mode {
	case "green":
		return trayGreen
	case "yellow":
		return trayYellow
	case "red":
		return trayRed
	}
	return trayEmpty
}

// trayFraction is the share of all CPUs the snapshot's mode lets Claude use:
// the CPUs of the mode's cpulist over those of the green one. 0 before the
// first apply; a list that does not parse counts as everything, so the icon
// never lies low.
func trayFraction(s sliceSnapshot) float64 {
	if s.Mode == "" {
		return 0
	}
	all, err1 := cpuParse(s.Cores["green"])
	mine, err2 := cpuParse(s.Cores[s.Mode])
	if err1 != nil || err2 != nil || len(all) == 0 {
		return 1
	}
	return math.Min(1, float64(len(mine))/float64(len(all)))
}

// trayLevel is the height of the level line above the circle's centre, in
// radii of the inner disc (-1 is the bottom, 1 the top), such that the part of
// the disc below it is frac of the disc's area. The area below a line at
// height h is r²(π - acos(h) + h·sqrt(1-h²)) for r = 1, which only grows with
// h's distance from the top, so a bisection finds it.
func trayLevel(frac float64) float64 {
	below := func(h float64) float64 { // share of the disc below height h
		return (math.Pi - math.Acos(h) + h*math.Sqrt(1-h*h)) / math.Pi
	}
	lo, hi := -1.0, 1.0
	for i := 0; i < 50; i++ {
		mid := (lo + hi) / 2
		if below(mid) < frac {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// trayDraw renders the gauge for a mode with frac of it filled. Anti-aliasing
// comes from the canvas oversampling.
//
// The level line sits just above the liquid, not centred on its edge, so it
// never eats into the filled area and the area stays exactly frac.
func trayDraw(n int, mode string, frac float64) *trayCanvas {
	cv := trayNewCanvas(n)
	cv.fill(trayDisc(trayDotR), trayOutline)
	cv.fill(trayDisc(trayDotInner), trayEmpty)
	if mode == "" || frac <= 0 {
		return cv
	}
	inner := trayDisc(trayDotInner)
	if frac >= 1 {
		cv.fill(inner, trayFillFor(mode))
		return cv
	}
	level := 0.5 - trayLevel(frac)*trayDotInner // y of the liquid's top edge, y grows downwards
	cv.fill(func(x, y float64) bool { return y >= level && inner(x, y) }, trayFillFor(mode))
	cv.fill(func(x, y float64) bool { return y < level && y >= level-trayDotStroke && inner(x, y) }, trayOutline)
	return cv
}

var traySizes = []int{22, 32, 48}

// trayPixmapsFor renders the ARGB32 pixmaps for a snapshot, at every size.
//
// Every call renders fresh memory, and it must stay that way. godbus/prop
// keeps a pointer to the value it was exported with and stores every later
// value *through* that pointer (prop.set → dbus.Store), which writes into the
// existing backing arrays rather than replacing them. A cached slice handed
// out here would alias the exported IconPixmap, and the next SetMust would
// overwrite the cached artwork with the new icon.
func trayPixmapsFor(s sliceSnapshot) []trayPixmap {
	frac := trayFraction(s)
	out := make([]trayPixmap, 0, len(traySizes))
	for _, size := range traySizes {
		out = append(out, trayDraw(size*trayOversample, s.Mode, frac).pixmap(size))
	}
	return out
}

// --- menu model ------------------------------------------------------------

// dbusmenu item ids: the root, then one radio row per mode (1, 2, 3).
const trayIDRoot int32 = 0

// trayIDMode is the row id of a mode, and trayModeOfID its inverse.
func trayIDMode(mode string) int32 {
	for i, m := range trayModes {
		if m == mode {
			return int32(i) + 1
		}
	}
	return -1
}

func trayModeOfID(id int32) (string, bool) {
	if id < 1 || int(id) > len(trayModes) {
		return "", false
	}
	return trayModes[id-1], true
}

// trayMenuNode is the dbusmenu layout struct: (ia{sv}av).
type trayMenuNode struct {
	ID       int32
	Props    map[string]dbus.Variant
	Children []dbus.Variant
}

// trayMenuProps is one entry of GetGroupProperties' a(ia{sv}).
type trayMenuProps struct {
	ID    int32
	Props map[string]dbus.Variant
}

// trayMenuRemovedProps is one entry of ItemsPropertiesUpdated's second
// argument, a(ias): the properties that went back to their default. We never
// remove one, but the signal's signature demands the array.
type trayMenuRemovedProps struct {
	ID    int32
	Props []string
}

// trayMenuEvent is one entry of EventGroup's a(isvu).
type trayMenuEvent struct {
	ID        int32
	EventID   string
	Data      dbus.Variant
	Timestamp uint32
}

// trayRow is one menu row before it becomes dbusmenu properties. The menu is
// flat — every row hangs off the menu bar — so the rows stay a comparable
// slice and trayRowsEqual can compare them with ==.
type trayRow struct {
	id      int32
	label   string
	enabled bool
	radio   bool // "toggle-type" = "radio"
	checked bool // the radio's "toggle-state"
}

func trayToggleState(checked bool) int32 {
	if checked {
		return 1
	}
	return 0
}

func (r trayRow) props() map[string]dbus.Variant {
	p := map[string]dbus.Variant{}
	// In dbusmenu "_" marks the following character as the mnemonic. Double it
	// to escape.
	p["label"] = dbus.MakeVariant(strings.ReplaceAll(r.label, "_", "__"))
	p["enabled"] = dbus.MakeVariant(r.enabled)
	p["visible"] = dbus.MakeVariant(true)
	if r.radio {
		p["toggle-type"] = dbus.MakeVariant("radio")
		p["toggle-state"] = dbus.MakeVariant(trayToggleState(r.checked))
	}
	return p
}

// --- tooltip and menu text -------------------------------------------------

// trayModeText is "Yellow: 3-9,13-19" for the tooltip; green says "all cores".
func trayModeText(s sliceSnapshot) string {
	if s.Mode == "" {
		return "not applied yet"
	}
	cores := s.Cores[s.Mode]
	switch {
	case s.Mode == "green" && cores != "":
		cores = "all cores (" + cores + ")"
	case s.Mode == "green":
		cores = "all cores"
	}
	if cores == "" {
		return trayModeName(s.Mode)
	}
	return trayModeName(s.Mode) + ": " + cores
}

func trayTitle(s sliceSnapshot) string { return "ccorral — " + trayModeText(s) }

func trayTooltipFor(s sliceSnapshot) trayTooltip {
	// Freshly built on every call, and it must stay that way: prop stores the
	// ToolTip through the pointer it already holds, so a cached value here
	// would alias the exported property exactly as trayPixmapsFor once did.
	return trayTooltip{
		IconName:    "",
		IconPixmap:  []trayPixmap{},
		Title:       trayTitle(s),
		Description: "",
	}
}

// trayRowsFor builds the whole menu: one radio per mode, the live one ticked.
func trayRowsFor(s sliceSnapshot) []trayRow {
	var rows []trayRow
	for _, m := range trayModes {
		label := trayModeName(m)
		cores := s.Cores[m]
		switch {
		case m == "green" && cores != "":
			label += " — all cores (" + cores + ")"
		case cores != "":
			label += " — " + cores
		}
		rows = append(rows, trayRow{id: trayIDMode(m), label: label, enabled: true, radio: true, checked: s.Mode == m})
	}
	return rows
}

func trayRowsEqual(a, b []trayRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// trayToggleOnlyDiff reports whether old and rows differ in nothing but the
// toggle-state of some rows, and if so returns just those rows' new state as
// ItemsPropertiesUpdated's a(ia{sv}).
//
// ok is false for an identical pair too: there is then nothing to send, and the
// caller only reaches this after establishing that something did change.
func trayToggleOnlyDiff(old, rows []trayRow) ([]trayMenuProps, bool) {
	if len(old) != len(rows) {
		return nil, false
	}
	var upd []trayMenuProps
	for i := range rows {
		if old[i] == rows[i] {
			continue
		}
		// Everything except checked has to match: compare the old row with its
		// checked flag set to the new one's, which leaves exactly that field out.
		was := old[i]
		was.checked = rows[i].checked
		if was != rows[i] {
			return nil, false
		}
		upd = append(upd, trayMenuProps{
			ID:    rows[i].id,
			Props: map[string]dbus.Variant{"toggle-state": dbus.MakeVariant(trayToggleState(rows[i].checked))},
		})
	}
	return upd, len(upd) > 0
}

// --- the tray itself -------------------------------------------------------

// trayCallTimeout bounds a call to the tray host, which runs on the update
// loop: a hung host must not stall icon updates for the bus's ~25 s default.
const trayCallTimeout = 2 * time.Second

type tray struct {
	ctx  context.Context // for bounding outgoing calls; cancelled on shutdown
	conn *dbus.Conn
	b    trayBackend
	name string // org.kde.StatusNotifierItem-<pid>-1

	// emitFn, when set, takes the place of conn.Emit. Tests use it to capture
	// which signal a change produced, without a bus.
	emitFn func(path dbus.ObjectPath, name string, args ...any)

	mu              sync.RWMutex
	snap            sliceSnapshot
	rows            []trayRow
	revision        uint32
	exported        bool
	warnedNoWatcher bool

	props *prop.Properties
}

// RunTray runs the StatusNotifierItem until ctx is done. A tray problem only
// logs: the daemon's job (pinning) does not depend on the icon. A missing or
// restarting tray host is not a problem either, we register again when the
// watcher shows up.
func RunTray(ctx context.Context, b trayBackend) {
	// Nothing to show if we are already shutting down.
	if ctx.Err() != nil {
		return
	}

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		log.Printf("tray: no session bus, running without a tray icon: %v", err)
		return
	}
	defer conn.Close()

	t := &tray{
		ctx:  ctx,
		conn: conn,
		b:    b,
		name: fmt.Sprintf("org.kde.StatusNotifierItem-%d-1", os.Getpid()),
	}

	// Watch for the tray host coming and going, so a shell restart, or the
	// service starting at login before the shell, registers (again).
	sigs := make(chan *dbus.Signal, 8)
	conn.Signal(sigs)
	defer conn.RemoveSignal(sigs)
	if err := conn.AddMatchSignal(
		dbus.WithMatchSender("org.freedesktop.DBus"),
		dbus.WithMatchObjectPath("/org/freedesktop/DBus"),
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, trayWatcherName),
	); err != nil {
		log.Printf("tray: cannot watch %s: %v", trayWatcherName, err)
	}

	changed := b.Changed()
	t.sync()

	for {
		select {
		case <-ctx.Done():
			t.teardown()
			return
		case <-changed:
			t.sync()
		case sig, ok := <-sigs:
			if !ok {
				// godbus closes the channel when the connection dies; without
				// this return the loop would spin on nil. No reconnect.
				log.Printf("tray: session bus gone, tray stopped")
				return
			}
			if sig == nil || sig.Name != "org.freedesktop.DBus.NameOwnerChanged" || len(sig.Body) < 3 {
				continue
			}
			newOwner, _ := sig.Body[2].(string)
			if newOwner == "" {
				continue // host went away; our objects stay put, it will be back
			}
			log.Printf("tray: %s appeared, registering", trayWatcherName)
			t.mu.Lock()
			t.warnedNoWatcher = false
			t.mu.Unlock()
			t.register()
		}
	}
}

// sync pulls the current snapshot, brings the exported objects in line with it
// and emits the change signals.
func (t *tray) sync() {
	snap := t.b.Snapshot()
	rows := trayRowsFor(snap)

	t.mu.Lock()
	old, oldRows := t.snap, t.rows
	first := oldRows == nil
	iconChanged := first || old.Mode != snap.Mode || trayFraction(old) != trayFraction(snap)
	tipChanged := first || trayTitle(old) != trayTitle(snap)
	menuChanged := first || !trayRowsEqual(oldRows, rows)
	t.snap, t.rows = snap, rows
	if menuChanged {
		t.revision++
	}
	rev := t.revision
	wasExported := t.exported
	t.mu.Unlock()

	if !wasExported && t.conn != nil {
		if err := t.export(); err != nil {
			log.Printf("tray: export failed: %v", err)
			return
		}
		t.emit(trayItemPath, trayItemIface+".NewStatus", "Active")
		t.register()
		return // export/register publishes the current values already
	}

	if t.props != nil {
		if iconChanged {
			t.props.SetMust(trayItemIface, "IconPixmap", trayPixmapsFor(snap))
		}
		if tipChanged {
			t.props.SetMust(trayItemIface, "ToolTip", trayTooltipFor(snap))
			t.props.SetMust(trayItemIface, "Title", trayTitle(snap))
		}
	}
	if iconChanged {
		t.emit(trayItemPath, trayItemIface+".NewIcon")
	}
	if tipChanged {
		t.emit(trayItemPath, trayItemIface+".NewToolTip")
		t.emit(trayItemPath, trayItemIface+".NewTitle")
	}
	if menuChanged {
		t.emitMenuChange(oldRows, rows, rev)
	}
}

// emitMenuChange tells the host what moved, in the smallest terms that say it.
//
// A radio moving changes nothing about the menu's shape, so it goes out as
// ItemsPropertiesUpdated carrying only the new toggle-state of the rows that
// flipped. LayoutUpdated is what a host answers by fetching the whole layout
// again, and some hosts close the open menu over it. Anything structural (a
// relabelled row after a core-group change) still needs LayoutUpdated. The
// revision is bumped either way.
func (t *tray) emitMenuChange(old, rows []trayRow, rev uint32) {
	if upd, ok := trayToggleOnlyDiff(old, rows); ok {
		t.emit(trayMenuPath, trayMenuIface+".ItemsPropertiesUpdated",
			upd, []trayMenuRemovedProps{})
		return
	}
	t.emit(trayMenuPath, trayMenuIface+".LayoutUpdated", rev, trayIDRoot)
}

func (t *tray) emit(path dbus.ObjectPath, name string, args ...any) {
	if t.emitFn != nil {
		t.emitFn(path, name, args...)
		return
	}
	if t.conn == nil {
		return
	}
	if err := t.conn.Emit(path, name, args...); err != nil {
		log.Printf("tray: emit %s: %v", name, err)
	}
}

// export claims the bus name and publishes both objects.
func (t *tray) export() error {
	t.mu.RLock()
	snap := t.snap
	t.mu.RUnlock()

	reply, err := t.conn.RequestName(t.name, dbus.NameFlagDoNotQueue)
	if err != nil {
		return fmt.Errorf("request name %s: %w", t.name, err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return fmt.Errorf("request name %s: not primary owner (%v)", t.name, reply)
	}

	props, err := prop.Export(t.conn, trayItemPath, prop.Map{
		trayItemIface: {
			"Category":   {Value: "SystemServices", Emit: prop.EmitConst},
			"Id":         {Value: "ccorral", Emit: prop.EmitConst},
			"Title":      {Value: trayTitle(snap), Emit: prop.EmitTrue},
			"Status":     {Value: "Active", Emit: prop.EmitTrue},
			"WindowId":   {Value: int32(0), Emit: prop.EmitConst},
			"ItemIsMenu": {Value: false, Emit: prop.EmitConst},
			"Menu":       {Value: trayMenuPath, Emit: prop.EmitConst},
			// IconName is empty on purpose, and never set. A host that sees a
			// non-empty name prefers it over IconPixmap and draws the theme's
			// icon instead of ours: Quickshell resolves the item to
			// "image://icon/<IconName>" whenever the name is set and only falls
			// back to the pixmap provider when it is empty. IconThemePath stays
			// empty for exactly the same reason.
			"IconName":          {Value: "", Emit: prop.EmitConst},
			"IconPixmap":        {Value: trayPixmapsFor(snap), Emit: prop.EmitTrue},
			"AttentionIconName": {Value: "", Emit: prop.EmitConst},
			"OverlayIconName":   {Value: "", Emit: prop.EmitConst},
			"IconThemePath":     {Value: "", Emit: prop.EmitConst},
			"ToolTip":           {Value: trayTooltipFor(snap), Emit: prop.EmitTrue},
		},
	})
	if err != nil {
		t.conn.ReleaseName(t.name)
		return fmt.Errorf("export item properties: %w", err)
	}
	t.props = props

	if _, err := prop.Export(t.conn, trayMenuPath, prop.Map{
		trayMenuIface: {
			"Version":       {Value: uint32(3), Emit: prop.EmitConst},
			"TextDirection": {Value: "ltr", Emit: prop.EmitConst},
			"Status":        {Value: "normal", Emit: prop.EmitTrue},
			"IconThemePath": {Value: []string{}, Emit: prop.EmitConst},
		},
	}); err != nil {
		t.conn.ReleaseName(t.name)
		return fmt.Errorf("export menu properties: %w", err)
	}

	if err := t.conn.Export(t, trayItemPath, trayItemIface); err != nil {
		t.conn.ReleaseName(t.name)
		return fmt.Errorf("export item: %w", err)
	}
	if err := t.conn.Export(t, trayMenuPath, trayMenuIface); err != nil {
		t.conn.ReleaseName(t.name)
		return fmt.Errorf("export menu: %w", err)
	}
	if err := t.conn.Export(introspect.NewIntrospectable(trayItemNode()), trayItemPath,
		"org.freedesktop.DBus.Introspectable"); err != nil {
		t.conn.ReleaseName(t.name)
		return fmt.Errorf("export item introspection: %w", err)
	}
	if err := t.conn.Export(introspect.NewIntrospectable(trayMenuNodeDesc()), trayMenuPath,
		"org.freedesktop.DBus.Introspectable"); err != nil {
		t.conn.ReleaseName(t.name)
		return fmt.Errorf("export menu introspection: %w", err)
	}

	t.mu.Lock()
	t.exported = true
	t.mu.Unlock()
	return nil
}

// register hands the bus name to the watcher. A missing watcher is logged once
// and otherwise ignored: the NameOwnerChanged match will catch it later.
func (t *tray) register() {
	t.mu.RLock()
	ok := t.exported
	warned := t.warnedNoWatcher
	t.mu.RUnlock()
	if !ok {
		return
	}
	base := t.ctx
	if base == nil {
		base = context.Background()
	}
	cctx, cancel := context.WithTimeout(base, trayCallTimeout)
	defer cancel()

	obj := t.conn.Object(trayWatcherName, trayWatcherPath)
	call := obj.CallWithContext(cctx, trayWatcherName+".RegisterStatusNotifierItem", 0, t.name)
	if call.Err != nil {
		if !warned {
			log.Printf("tray: no %s yet (%v); waiting for a tray host", trayWatcherName, call.Err)
			t.mu.Lock()
			t.warnedNoWatcher = true
			t.mu.Unlock()
		}
		return
	}
	log.Printf("tray: registered %s with %s", t.name, trayWatcherName)
}

// teardown unexports everything and drops the bus name, so watchers see the
// name vanish and forget us.
func (t *tray) teardown() {
	t.mu.Lock()
	if !t.exported {
		t.mu.Unlock()
		return
	}
	t.exported = false
	t.props = nil
	t.mu.Unlock()

	for _, p := range []struct {
		path  dbus.ObjectPath
		iface string
	}{
		{trayItemPath, trayItemIface},
		{trayItemPath, "org.freedesktop.DBus.Properties"},
		{trayItemPath, "org.freedesktop.DBus.Introspectable"},
		{trayMenuPath, trayMenuIface},
		{trayMenuPath, "org.freedesktop.DBus.Properties"},
		{trayMenuPath, "org.freedesktop.DBus.Introspectable"},
	} {
		if err := t.conn.Export(nil, p.path, p.iface); err != nil {
			log.Printf("tray: unexport %s %s: %v", p.path, p.iface, err)
		}
	}
	if _, err := t.conn.ReleaseName(t.name); err != nil {
		log.Printf("tray: release %s: %v", t.name, err)
	}
}

// --- org.kde.StatusNotifierItem methods ------------------------------------

// setMode applies a mode picked from the tray. The tray itself updates when
// the backend signals Changed, not here.
func (t *tray) setMode(mode string) {
	if err := t.b.SetMode(mode); err != nil {
		log.Printf("tray: set mode %s: %v", mode, err)
	}
}

// Activate is the left click (ItemIsMenu is false, so hosts send this): cycle
// Green, Yellow, Red.
func (t *tray) Activate(x, y int32) *dbus.Error {
	go t.setMode(trayNext(t.b.Snapshot().Mode))
	return nil
}

// SecondaryActivate is accepted and ignored.
func (t *tray) SecondaryActivate(x, y int32) *dbus.Error { return nil }

// ContextMenu exists only so hosts that insist on calling it get a reply; the
// actual menu is the dbusmenu object named by the Menu property.
func (t *tray) ContextMenu(x, y int32) *dbus.Error { return nil }

// Scroll is accepted and ignored.
func (t *tray) Scroll(delta int32, orientation string) *dbus.Error { return nil }

// --- com.canonical.dbusmenu methods ----------------------------------------

func (t *tray) layout() (uint32, []trayRow) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.revision, t.rows
}

func (t *tray) rowByID(id int32) (trayRow, bool) {
	_, rows := t.layout()
	for _, r := range rows {
		if r.id == id {
			return r, true
		}
	}
	return trayRow{}, false
}

func trayFilter(p map[string]dbus.Variant, names []string) map[string]dbus.Variant {
	if len(names) == 0 {
		return p
	}
	out := make(map[string]dbus.Variant, len(names))
	for _, n := range names {
		if v, ok := p[n]; ok {
			out[n] = v
		}
	}
	return out
}

// trayNodeFor builds one node. The menu is flat, so no row has children and
// the dbusmenu recursionDepth never matters below the root.
func trayNodeFor(r trayRow, propertyNames []string) trayMenuNode {
	return trayMenuNode{ID: r.id, Props: trayFilter(r.props(), propertyNames), Children: []dbus.Variant{}}
}

// GetLayout implements com.canonical.dbusmenu.GetLayout.
func (t *tray) GetLayout(parentID, recursionDepth int32, propertyNames []string) (uint32, trayMenuNode, *dbus.Error) {
	rev, rows := t.layout()

	if parentID != trayIDRoot {
		for _, r := range rows {
			if r.id == parentID {
				return rev, trayNodeFor(r, propertyNames), nil
			}
		}
		return rev, trayMenuNode{}, dbus.NewError("com.canonical.dbusmenu.Error.InvalidId", []any{"no such item"})
	}

	root := trayMenuNode{
		ID: trayIDRoot,
		Props: trayFilter(map[string]dbus.Variant{
			"children-display": dbus.MakeVariant("submenu"),
		}, propertyNames),
		Children: []dbus.Variant{},
	}
	if recursionDepth != 0 {
		for _, r := range rows {
			root.Children = append(root.Children, dbus.MakeVariant(trayNodeFor(r, propertyNames)))
		}
	}
	return rev, root, nil
}

// GetGroupProperties implements com.canonical.dbusmenu.GetGroupProperties. An
// empty id list means every item.
func (t *tray) GetGroupProperties(ids []int32, propertyNames []string) ([]trayMenuProps, *dbus.Error) {
	_, rows := t.layout()
	out := []trayMenuProps{}
	want := func(id int32) bool {
		if len(ids) == 0 {
			return true
		}
		for _, i := range ids {
			if i == id {
				return true
			}
		}
		return false
	}
	if want(trayIDRoot) {
		out = append(out, trayMenuProps{ID: trayIDRoot, Props: trayFilter(
			map[string]dbus.Variant{"children-display": dbus.MakeVariant("submenu")}, propertyNames)})
	}
	for _, r := range rows {
		if want(r.id) {
			out = append(out, trayMenuProps{ID: r.id, Props: trayFilter(r.props(), propertyNames)})
		}
	}
	return out, nil
}

// GetProperty implements com.canonical.dbusmenu.GetProperty.
func (t *tray) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	var props map[string]dbus.Variant
	if id == trayIDRoot {
		props = map[string]dbus.Variant{"children-display": dbus.MakeVariant("submenu")}
	} else {
		r, ok := t.rowByID(id)
		if !ok {
			return dbus.Variant{}, dbus.NewError("com.canonical.dbusmenu.Error.InvalidId", []any{"no such item"})
		}
		props = r.props()
	}
	v, ok := props[name]
	if !ok {
		return dbus.Variant{}, dbus.NewError("com.canonical.dbusmenu.Error.InvalidProperty", []any{name})
	}
	return v, nil
}

// Event implements com.canonical.dbusmenu.Event: a click on a mode row sets it.
func (t *tray) Event(id int32, eventID string, data dbus.Variant, timestamp uint32) *dbus.Error {
	if eventID != "clicked" {
		return nil
	}
	if mode, ok := trayModeOfID(id); ok {
		go t.setMode(mode)
	}
	return nil
}

// EventGroup implements com.canonical.dbusmenu.EventGroup. It returns the ids
// it did not recognise, as the spec requires.
func (t *tray) EventGroup(events []trayMenuEvent) ([]int32, *dbus.Error) {
	bad := []int32{}
	for _, e := range events {
		if _, ok := t.rowByID(e.ID); !ok && e.ID != trayIDRoot {
			bad = append(bad, e.ID)
			continue
		}
		t.Event(e.ID, e.EventID, e.Data, e.Timestamp)
	}
	return bad, nil
}

// AboutToShow implements com.canonical.dbusmenu.AboutToShow. The layout is
// always current, so nothing needs refreshing before the menu pops up.
func (t *tray) AboutToShow(id int32) (bool, *dbus.Error) { return false, nil }

// AboutToShowGroup implements com.canonical.dbusmenu.AboutToShowGroup.
func (t *tray) AboutToShowGroup(ids []int32) ([]int32, []int32, *dbus.Error) {
	return []int32{}, []int32{}, nil
}

// --- introspection ---------------------------------------------------------

func trayItemNode() *introspect.Node {
	return &introspect.Node{
		Name: string(trayItemPath),
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			{
				Name: trayItemIface,
				Methods: []introspect.Method{
					{Name: "Activate", Args: []introspect.Arg{{Name: "x", Type: "i", Direction: "in"}, {Name: "y", Type: "i", Direction: "in"}}},
					{Name: "SecondaryActivate", Args: []introspect.Arg{{Name: "x", Type: "i", Direction: "in"}, {Name: "y", Type: "i", Direction: "in"}}},
					{Name: "ContextMenu", Args: []introspect.Arg{{Name: "x", Type: "i", Direction: "in"}, {Name: "y", Type: "i", Direction: "in"}}},
					{Name: "Scroll", Args: []introspect.Arg{{Name: "delta", Type: "i", Direction: "in"}, {Name: "orientation", Type: "s", Direction: "in"}}},
				},
				Signals: []introspect.Signal{
					{Name: "NewIcon"},
					{Name: "NewAttentionIcon"},
					{Name: "NewOverlayIcon"},
					{Name: "NewToolTip"},
					{Name: "NewTitle"},
					{Name: "NewStatus", Args: []introspect.Arg{{Name: "status", Type: "s"}}},
				},
				Properties: []introspect.Property{
					{Name: "Category", Type: "s", Access: "read"},
					{Name: "Id", Type: "s", Access: "read"},
					{Name: "Title", Type: "s", Access: "read"},
					{Name: "Status", Type: "s", Access: "read"},
					{Name: "WindowId", Type: "i", Access: "read"},
					{Name: "ItemIsMenu", Type: "b", Access: "read"},
					{Name: "Menu", Type: "o", Access: "read"},
					{Name: "IconName", Type: "s", Access: "read"},
					{Name: "IconPixmap", Type: "a(iiay)", Access: "read"},
					{Name: "AttentionIconName", Type: "s", Access: "read"},
					{Name: "OverlayIconName", Type: "s", Access: "read"},
					{Name: "IconThemePath", Type: "s", Access: "read"},
					{Name: "ToolTip", Type: "(sa(iiay)ss)", Access: "read"},
				},
			},
		},
	}
}

func trayMenuNodeDesc() *introspect.Node {
	return &introspect.Node{
		Name: string(trayMenuPath),
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			{
				Name: trayMenuIface,
				Methods: []introspect.Method{
					{Name: "GetLayout", Args: []introspect.Arg{
						{Name: "parentId", Type: "i", Direction: "in"},
						{Name: "recursionDepth", Type: "i", Direction: "in"},
						{Name: "propertyNames", Type: "as", Direction: "in"},
						{Name: "revision", Type: "u", Direction: "out"},
						{Name: "layout", Type: "(ia{sv}av)", Direction: "out"},
					}},
					{Name: "GetGroupProperties", Args: []introspect.Arg{
						{Name: "ids", Type: "ai", Direction: "in"},
						{Name: "propertyNames", Type: "as", Direction: "in"},
						{Name: "properties", Type: "a(ia{sv})", Direction: "out"},
					}},
					{Name: "GetProperty", Args: []introspect.Arg{
						{Name: "id", Type: "i", Direction: "in"},
						{Name: "name", Type: "s", Direction: "in"},
						{Name: "value", Type: "v", Direction: "out"},
					}},
					{Name: "Event", Args: []introspect.Arg{
						{Name: "id", Type: "i", Direction: "in"},
						{Name: "eventId", Type: "s", Direction: "in"},
						{Name: "data", Type: "v", Direction: "in"},
						{Name: "timestamp", Type: "u", Direction: "in"},
					}},
					{Name: "EventGroup", Args: []introspect.Arg{
						{Name: "events", Type: "a(isvu)", Direction: "in"},
						{Name: "idErrors", Type: "ai", Direction: "out"},
					}},
					{Name: "AboutToShow", Args: []introspect.Arg{
						{Name: "id", Type: "i", Direction: "in"},
						{Name: "needUpdate", Type: "b", Direction: "out"},
					}},
					{Name: "AboutToShowGroup", Args: []introspect.Arg{
						{Name: "ids", Type: "ai", Direction: "in"},
						{Name: "updatesNeeded", Type: "ai", Direction: "out"},
						{Name: "idErrors", Type: "ai", Direction: "out"},
					}},
				},
				Signals: []introspect.Signal{
					{Name: "ItemsPropertiesUpdated", Args: []introspect.Arg{
						{Name: "updatedProps", Type: "a(ia{sv})"},
						{Name: "removedProps", Type: "a(ias)"},
					}},
					{Name: "LayoutUpdated", Args: []introspect.Arg{
						{Name: "revision", Type: "u"},
						{Name: "parent", Type: "i"},
					}},
					{Name: "ItemActivationRequested", Args: []introspect.Arg{
						{Name: "id", Type: "i"},
						{Name: "timestamp", Type: "u"},
					}},
				},
				Properties: []introspect.Property{
					{Name: "Version", Type: "u", Access: "read"},
					{Name: "TextDirection", Type: "s", Access: "read"},
					{Name: "Status", Type: "s", Access: "read"},
					{Name: "IconThemePath", Type: "as", Access: "read"},
				},
			},
		},
	}
}
