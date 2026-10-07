package main

// tray_test.go never touches the session bus: connecting would claim a name
// and make a real icon appear in the user's panel. Everything here is pure Go
// against a fake backend, including the reproduction of what godbus/prop does
// to a property value, which needs no connection at all.

import (
	"bytes"
	"context"
	"errors"
	"image/color"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// fakeTrayBackend is a trayBackend whose SetMode moves its mode, records the
// call and signals Changed, like the real one after a successful apply.
type fakeTrayBackend struct {
	mu      sync.Mutex
	snap    sliceSnapshot
	err     error
	changed chan struct{}
	calls   chan string
}

func newFakeTrayBackend(mode string) *fakeTrayBackend {
	return &fakeTrayBackend{
		snap: sliceSnapshot{Mode: mode, Cores: map[string]string{
			"green": "0-19", "yellow": "3-9,13-19", "red": "7-9,17-19",
		}},
		changed: make(chan struct{}, 1),
		calls:   make(chan string, 16),
	}
}

func (f *fakeTrayBackend) Snapshot() sliceSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := sliceSnapshot{Mode: f.snap.Mode, Theme: f.snap.Theme, Cores: map[string]string{}}
	for k, v := range f.snap.Cores {
		s.Cores[k] = v
	}
	return s
}

func (f *fakeTrayBackend) SetMode(mode string) error {
	f.calls <- mode
	if f.err != nil {
		return f.err
	}
	f.set(mode)
	return nil
}

// set changes the mode as a CLI `ccorral red` would.
func (f *fakeTrayBackend) set(mode string) {
	f.mu.Lock()
	f.snap.Mode = mode
	f.mu.Unlock()
	select {
	case f.changed <- struct{}{}:
	default:
	}
}

// setTheme changes the theme as a reload of an edited config would.
func (f *fakeTrayBackend) setTheme(theme string) {
	f.mu.Lock()
	f.snap.Theme = theme
	f.mu.Unlock()
	select {
	case f.changed <- struct{}{}:
	default:
	}
}

func (f *fakeTrayBackend) Changed() <-chan struct{} { return f.changed }

func (f *fakeTrayBackend) nextCall(t *testing.T) string {
	t.Helper()
	select {
	case m := <-f.calls:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("SetMode was not called")
		return ""
	}
}

// emitCall is one captured D-Bus signal.
type emitCall struct {
	path dbus.ObjectPath
	name string
	args []any
}

// newTestTray is a bus-free tray over a fake backend that records the signals
// it would send. It has synced once, so the first-pass signals are discarded.
func newTestTray(t *testing.T, mode string) (*tray, *fakeTrayBackend, func() []emitCall) {
	t.Helper()
	b := newFakeTrayBackend(mode)
	tr := &tray{b: b}
	var mu sync.Mutex
	var calls []emitCall
	tr.emitFn = func(path dbus.ObjectPath, name string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, emitCall{path, name, args})
	}
	tr.sync()
	mu.Lock()
	calls = nil
	mu.Unlock()
	return tr, b, func() []emitCall {
		mu.Lock()
		defer mu.Unlock()
		out := append([]emitCall(nil), calls...)
		calls = nil
		return out
	}
}

func names(cs []emitCall) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.name)
	}
	return out
}

// --- pixmaps ---------------------------------------------------------------

func snapOf(mode string) sliceSnapshot {
	return sliceSnapshot{Mode: mode, Cores: map[string]string{
		"green": "0-19", "yellow": "3-9,13-19", "red": "7-9,17-19",
	}}
}

// TestTrayFraction: the filled share is the mode's CPU count over green's, and
// follows the groups when `ccorral settings` changes them.
func TestTrayFraction(t *testing.T) {
	for _, c := range []struct {
		s    sliceSnapshot
		want float64
	}{
		{snapOf("green"), 1},
		{snapOf("yellow"), 0.7},
		{snapOf("red"), 0.3},
		{snapOf(""), 0},
		{sliceSnapshot{Mode: "yellow", Cores: map[string]string{"green": "0-19", "yellow": "2-9"}}, 0.4},
		{sliceSnapshot{Mode: "yellow", Cores: map[string]string{"green": "0-19", "yellow": "x"}}, 1},
		{sliceSnapshot{Mode: "yellow"}, 1},
	} {
		if got := trayFraction(c.s); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("trayFraction(%v) = %v, want %v", c.s, got, c.want)
		}
	}
}

// TestTrayDrawAreas samples a large canvas: the liquid must cover frac of the
// inner disc (the level line does not eat into it), be the mode's colour at
// the bottom and transparent at the top.
func TestTrayDrawAreas(t *testing.T) {
	const n = 600
	inner := trayDisc(trayDotInner)
	for _, mode := range []string{"green", "yellow", "red"} {
		for _, frac := range []float64{0.1, 0.3, 0.5, 0.7, 0.9, 1} {
			cv := trayDraw(n, mode, frac, trayBlack)
			var disc, liquid int
			for yi := 0; yi < n; yi++ {
				for xi := 0; xi < n; xi++ {
					x, y := (float64(xi)+0.5)/n, (float64(yi)+0.5)/n
					if inner(x, y) {
						disc++
					}
					if cv.px[yi*n+xi] == trayFillFor(mode) {
						liquid++
					}
				}
			}
			if got := float64(liquid) / float64(disc); math.Abs(got-frac) > 0.05 {
				t.Errorf("%s at %.1f: liquid covers %.3f of the disc", mode, frac, got)
			}
			if cv.px[(n-1-n/6)*n+n/2] != trayFillFor(mode) {
				t.Errorf("%s at %.1f: bottom is not the mode colour", mode, frac)
			}
		}
	}
}

// px is the ARGB of the pixel (x, y) of the size px pixmap of a snapshot.
func px(t *testing.T, s sliceSnapshot, size, x, y int) [4]byte {
	t.Helper()
	for _, p := range trayPixmapsFor(s) {
		if int(p.Width) == size && int(p.Height) == size {
			if len(p.Data) != size*size*4 {
				t.Fatalf("%d px pixmap has %d bytes, want %d", size, len(p.Data), size*size*4)
			}
			o := (y*size + x) * 4
			return [4]byte{p.Data[o], p.Data[o+1], p.Data[o+2], p.Data[o+3]}
		}
	}
	t.Fatalf("no %d px pixmap", size)
	return [4]byte{}
}

func argb(c color.RGBA) [4]byte { return [4]byte{c.A, c.R, c.G, c.B} }

// TestTrayPixmapSamples: in the real pixmaps (ARGB order) a pixel near the
// bottom is the mode colour, one near the top fully transparent (green is
// full, so green is the exception), "not applied yet" is the outline only, the
// outline follows the theme and the corner is transparent.
func TestTrayPixmapSamples(t *testing.T) {
	for _, theme := range []string{themeLight, themeDark} {
		ink := trayBlack
		if theme == themeDark {
			ink = trayWhite
		}
		for _, size := range []int{22, 32, 48} {
			x, bottom, top := size/2, size*83/100, size*17/100
			for _, c := range []struct {
				mode       string
				bot, upper color.RGBA
			}{
				{"green", trayGreen, trayGreen},
				{"yellow", trayYellow, trayClear},
				{"red", trayRed, trayClear},
				{"", trayClear, trayClear},
			} {
				s := snapOf(c.mode)
				s.Theme = theme
				if got := px(t, s, size, x, bottom); got != argb(c.bot) {
					t.Errorf("%s %q %d px: bottom = %x, want %x", theme, c.mode, size, got, argb(c.bot))
				}
				if got := px(t, s, size, x, top); got != argb(c.upper) {
					t.Errorf("%s %q %d px: top = %x, want %x", theme, c.mode, size, got, argb(c.upper))
				}
				if got := px(t, s, size, 0, 0); got[0] != 0 {
					t.Errorf("%s %q %d px: corner alpha = %#x, want transparent", theme, c.mode, size, got[0])
				}
				// From the left edge on the middle row, the first visible
				// pixel is the (anti-aliased) outline in the theme's colour.
				for xx := 0; xx < size; xx++ {
					if p := px(t, s, size, xx, size/2); p[0] != 0 {
						for _, ch := range p[1:] {
							if d := int(ch) - int(ink.R); d > 0x40 || d < -0x40 {
								t.Errorf("%s %q %d px: outermost pixel = %x, want %x", theme, c.mode, size, p, argb(ink))
								break
							}
						}
						break
					}
				}
			}
		}
	}
	// Yellow and red differ in level, so their 22 px icons differ.
	modes := []string{"", "green", "yellow", "red"}
	for i, a := range modes {
		for _, b := range modes[i+1:] {
			if pixmapsEqual(trayPixmapsFor(snapOf(a)), trayPixmapsFor(snapOf(b))) {
				t.Errorf("%q and %q render identically", a, b)
			}
		}
	}
}

// TestTrayThemeOutline: the outline and the level line are black for light
// (also the default, "") and white for dark; "" mode is the outline circle
// alone, so its inside stays empty; the themes differ.
func TestTrayThemeOutline(t *testing.T) {
	const n = 600
	for _, theme := range []string{"", themeLight, themeDark} {
		ink := trayBlack
		if theme == themeDark {
			ink = trayWhite
		}
		if got := trayOutlineFor(theme); got != ink {
			t.Errorf("outline(%q) = %v, want %v", theme, got, ink)
		}
		// Outline ring: 0.43 is between the inner (0.39) and outer (0.47) radius.
		cv := trayDraw(n, "yellow", 0.7, ink)
		ring := (n/2)*n + n*7/100
		if cv.px[ring] != ink {
			t.Errorf("%q: ring sample = %v, want outline", theme, cv.px[ring])
		}
		// Level line: just above the liquid's top edge on the centre column.
		level := 0.5 - trayLevel(0.7)*trayDotInner
		if got := cv.px[int((level-trayDotStroke/2)*n)*n+n/2]; got != ink {
			t.Errorf("%q: level line sample = %v, want outline colour", theme, got)
		}
		// "" mode: nothing but the ring.
		cv = trayDraw(n, "", 0, ink)
		for yi := 0; yi < n; yi++ {
			for xi := 0; xi < n; xi++ {
				d := math.Hypot((float64(xi)+0.5)/n-0.5, (float64(yi)+0.5)/n-0.5)
				if math.Abs(d-trayDotInner) < 0.002 || math.Abs(d-trayDotR) < 0.002 {
					continue // boundary samples: rounding
				}
				want := trayClear
				if d > trayDotInner && d < trayDotR {
					want = ink
				}
				if cv.px[yi*n+xi] != want {
					t.Fatalf("%q: empty gauge sample (%d,%d) = %v, want %v", theme, xi, yi, cv.px[yi*n+xi], want)
				}
			}
		}
	}
	light, dark := snapOf("yellow"), snapOf("yellow")
	dark.Theme = themeDark
	if pixmapsEqual(trayPixmapsFor(light), trayPixmapsFor(dark)) {
		t.Error("light and dark render identically")
	}
}

// TestTrayPixmapsFresh: two calls share no memory.
func TestTrayPixmapsFresh(t *testing.T) {
	a, b := trayPixmapsFor(snapOf("yellow")), trayPixmapsFor(snapOf("yellow"))
	want := clonePixmaps(b)
	a[0].Data[0] ^= 0xff
	if !pixmapsEqual(b, want) {
		t.Error("mutating one result changed another")
	}
}

// TestTrayPixmapsSurviveAPropStore reproduces what godbus/prop does without a
// bus: it keeps *[]trayPixmap pointing at the exported value and stores each
// new value through that pointer with dbus.Store, writing into the existing
// backing arrays. What it was handed must not be shared with anything else.
func TestTrayPixmapsSurviveAPropStore(t *testing.T) {
	greenBefore := clonePixmaps(trayPixmapsFor(snapOf("green")))

	exported := reflect.New(reflect.TypeOf([]trayPixmap(nil)))
	exported.Elem().Set(reflect.ValueOf(trayPixmapsFor(snapOf("green"))))

	if err := dbus.Store([]any{trayPixmapsFor(snapOf("red"))}, exported.Interface()); err != nil {
		t.Fatalf("dbus.Store: %v", err)
	}

	if !pixmapsEqual(trayPixmapsFor(snapOf("green")), greenBefore) {
		t.Error("publishing the red icon corrupted the green icon")
	}
	if pixmapsEqual(trayPixmapsFor(snapOf("green")), trayPixmapsFor(snapOf("red"))) {
		t.Error("green and red pixmaps are now identical")
	}
}

func clonePixmaps(ps []trayPixmap) []trayPixmap {
	out := make([]trayPixmap, len(ps))
	for i, p := range ps {
		out[i] = trayPixmap{Width: p.Width, Height: p.Height, Data: append([]byte(nil), p.Data...)}
	}
	return out
}

func pixmapsEqual(a, b []trayPixmap) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Width != b[i].Width || a[i].Height != b[i].Height || !bytes.Equal(a[i].Data, b[i].Data) {
			return false
		}
	}
	return true
}

// --- menu ------------------------------------------------------------------

func nodeChildren(n trayMenuNode) []trayMenuNode {
	out := make([]trayMenuNode, 0, len(n.Children))
	for _, c := range n.Children {
		out = append(out, c.Value().(trayMenuNode))
	}
	return out
}

func propOf(t *testing.T, n trayMenuNode, name string) any {
	t.Helper()
	v, ok := n.Props[name]
	if !ok {
		t.Fatalf("item %d has no %q property (props: %v)", n.ID, name, n.Props)
	}
	return v.Value()
}

// TestTrayMenuLayout pins the whole menu: three flat radio rows, the live one
// ticked, nothing else.
func TestTrayMenuLayout(t *testing.T) {
	tr, _, _ := newTestTray(t, "yellow")

	_, root, derr := tr.GetLayout(trayIDRoot, -1, nil)
	if derr != nil {
		t.Fatalf("GetLayout: %v", derr)
	}
	top := nodeChildren(root)
	wantLabels := []string{"Green — all cores (0-19)", "Yellow — 3-9,13-19", "Red — 7-9,17-19"}
	if len(top) != len(wantLabels) {
		t.Fatalf("%d root children, want %d", len(top), len(wantLabels))
	}
	for i, n := range top {
		if n.ID != int32(i)+1 {
			t.Errorf("row %d has id %d", i, n.ID)
		}
		if got := propOf(t, n, "label"); got != wantLabels[i] {
			t.Errorf("row %d label = %q, want %q", i, got, wantLabels[i])
		}
		if got := propOf(t, n, "toggle-type"); got != "radio" {
			t.Errorf("row %d toggle-type = %v, want radio", i, got)
		}
		wantState := int32(0)
		if i == 1 {
			wantState = 1
		}
		if got := propOf(t, n, "toggle-state"); got != wantState {
			t.Errorf("row %d toggle-state = %v, want %d", i, got, wantState)
		}
		if got := propOf(t, n, "enabled"); got != true {
			t.Errorf("row %d enabled = %v", i, got)
		}
		if len(n.Children) != 0 {
			t.Errorf("row %d has children; the menu is flat", i)
		}
	}

	// Depth 0 asks for the root alone; an unknown parent is an error.
	if _, shallow, _ := tr.GetLayout(trayIDRoot, 0, nil); len(shallow.Children) != 0 {
		t.Errorf("depth 0 returned %d children", len(shallow.Children))
	}
	if _, _, derr := tr.GetLayout(99, -1, nil); derr == nil {
		t.Error("GetLayout of an unknown id succeeded")
	}
	props, _ := tr.GetGroupProperties(nil, []string{"label"})
	if len(props) != 4 { // root + 3 rows
		t.Errorf("GetGroupProperties(nil) = %d entries, want 4", len(props))
	}
}

// TestTrayMenuNoModeYet: before the first apply nothing is ticked.
func TestTrayMenuNoModeYet(t *testing.T) {
	tr, _, _ := newTestTray(t, "")
	for _, r := range tr.rows {
		if r.checked {
			t.Errorf("row %q is ticked with no mode applied", r.label)
		}
	}
}

// TestTrayLabelEscapesUnderscore: "_" is dbusmenu's mnemonic marker and must
// be doubled.
func TestTrayLabelEscapesUnderscore(t *testing.T) {
	p := trayRow{id: 1, label: "a_b", enabled: true}.props()
	if got := p["label"].Value(); got != "a__b" {
		t.Errorf("label = %q, want a__b", got)
	}
}

// TestTrayTooltipText covers the tooltip and title of every mode.
func TestTrayTooltipText(t *testing.T) {
	cores := map[string]string{"green": "0-19", "yellow": "3-9,13-19", "red": "7-9,17-19"}
	for mode, want := range map[string]string{
		"green":  "ccorral — Green: all cores (0-19)",
		"yellow": "ccorral — Yellow: 3-9,13-19",
		"red":    "ccorral — Red: 7-9,17-19",
		"":       "ccorral — not applied yet",
	} {
		s := sliceSnapshot{Mode: mode, Cores: cores}
		if got := trayTitle(s); got != want {
			t.Errorf("title(%q) = %q, want %q", mode, got, want)
		}
		if got := trayTooltipFor(s).Title; got != want {
			t.Errorf("tooltip(%q) = %q, want %q", mode, got, want)
		}
	}
	if got := trayTitle(sliceSnapshot{Mode: "green"}); got != "ccorral — Green: all cores" {
		t.Errorf("green without cores: %q", got)
	}
}

// --- clicks ----------------------------------------------------------------

// TestTrayActivateCycles: left-click goes Green, Yellow, Red, Green, and from
// "not applied yet" to Green.
func TestTrayActivateCycles(t *testing.T) {
	for _, c := range []struct{ from, to string }{
		{"green", "yellow"}, {"yellow", "red"}, {"red", "green"}, {"", "green"},
	} {
		tr, b, _ := newTestTray(t, c.from)
		if derr := tr.Activate(0, 0); derr != nil {
			t.Fatalf("Activate: %v", derr)
		}
		if got := b.nextCall(t); got != c.to {
			t.Errorf("click in %q set %q, want %q", c.from, got, c.to)
		}
	}
}

func TestTrayMenuClickSetsMode(t *testing.T) {
	tr, b, _ := newTestTray(t, "green")
	tr.Event(3, "clicked", dbus.MakeVariant(""), 0)
	if got := b.nextCall(t); got != "red" {
		t.Errorf("clicking the Red row set %q", got)
	}

	// Anything but "clicked", and ids that are no mode row, do nothing.
	tr.Event(1, "hovered", dbus.MakeVariant(""), 0)
	tr.Event(0, "clicked", dbus.MakeVariant(""), 0)
	tr.Event(4, "clicked", dbus.MakeVariant(""), 0)

	bad, _ := tr.EventGroup([]trayMenuEvent{
		{ID: 2, EventID: "clicked"}, {ID: 77, EventID: "clicked"},
	})
	if !reflect.DeepEqual(bad, []int32{77}) {
		t.Errorf("EventGroup bad ids = %v, want [77]", bad)
	}
	if got := b.nextCall(t); got != "yellow" {
		t.Errorf("EventGroup click set %q, want yellow", got)
	}
	select {
	case m := <-b.calls:
		t.Errorf("unexpected extra SetMode(%q)", m)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestTraySetModeErrorOnlyLogs: a failing backend must not panic or wedge.
func TestTraySetModeErrorOnlyLogs(t *testing.T) {
	tr, b, _ := newTestTray(t, "green")
	b.err = errors.New("boom")
	tr.Activate(0, 0)
	if got := b.nextCall(t); got != "yellow" {
		t.Errorf("set %q", got)
	}
}

// --- updates ---------------------------------------------------------------

// TestTraySyncUpdatesAndSignals: a mode change from outside (the CLI) moves
// icon, tooltip and title, and the ticks go out as ItemsPropertiesUpdated, not
// as a full LayoutUpdated.
func TestTraySyncUpdatesAndSignals(t *testing.T) {
	tr, b, got := newTestTray(t, "green")
	rev := tr.revision

	b.set("red")
	<-b.Changed()
	tr.sync()

	calls := got()
	want := []string{
		trayItemIface + ".NewIcon",
		trayItemIface + ".NewToolTip",
		trayItemIface + ".NewTitle",
		trayMenuIface + ".ItemsPropertiesUpdated",
	}
	if !reflect.DeepEqual(names(calls), want) {
		t.Fatalf("emitted %v, want %v", names(calls), want)
	}
	if tr.revision != rev+1 {
		t.Errorf("revision = %d, want %d", tr.revision, rev+1)
	}
	upd, ok := calls[3].args[0].([]trayMenuProps)
	if !ok || len(upd) != 2 {
		t.Fatalf("ItemsPropertiesUpdated args = %#v, want two rows", calls[3].args)
	}
	if upd[0].ID != 1 || upd[0].Props["toggle-state"].Value() != int32(0) ||
		upd[1].ID != 3 || upd[1].Props["toggle-state"].Value() != int32(1) {
		t.Errorf("updated props = %+v", upd)
	}
	if _, ok := calls[3].args[1].([]trayMenuRemovedProps); !ok {
		t.Errorf("second argument is %T, want []trayMenuRemovedProps", calls[3].args[1])
	}
	if tr.snap.Mode != "red" || trayTitle(tr.snap) != "ccorral — Red: 7-9,17-19" {
		t.Errorf("state not updated: %+v", tr.snap)
	}

	// Nothing changed: nothing emitted, revision untouched.
	tr.sync()
	if calls := got(); len(calls) != 0 {
		t.Errorf("emitted %v on an unchanged snapshot", names(calls))
	}
}

// TestTraySyncThemeOnly: a theme change alone redraws the icon, and nothing
// else (title, tooltip and menu text do not depend on it).
func TestTraySyncThemeOnly(t *testing.T) {
	tr, b, got := newTestTray(t, "yellow")
	b.setTheme(themeDark)
	<-b.Changed()
	tr.sync()
	if want := []string{trayItemIface + ".NewIcon"}; !reflect.DeepEqual(names(got()), want) {
		t.Errorf("emitted something other than %v", want)
	}
	if tr.snap.Theme != themeDark {
		t.Errorf("snap theme = %q", tr.snap.Theme)
	}
}

// TestTrayLayoutUpdatedOnStructuralChange: new core groups relabel the rows,
// which a host can only learn by fetching the layout again.
func TestTrayLayoutUpdatedOnStructuralChange(t *testing.T) {
	tr, b, got := newTestTray(t, "yellow")
	b.mu.Lock()
	b.snap.Cores["yellow"] = "2-9"
	b.mu.Unlock()

	tr.sync()

	calls := got()
	if len(calls) == 0 || calls[len(calls)-1].name != trayMenuIface+".LayoutUpdated" {
		t.Fatalf("emitted %v, want a final LayoutUpdated", names(calls))
	}
	var newIcon bool
	for _, c := range calls {
		if c.name == trayMenuIface+".ItemsPropertiesUpdated" {
			t.Error("a relabelled row went out as ItemsPropertiesUpdated")
		}
		newIcon = newIcon || c.name == trayItemIface+".NewIcon"
	}
	if !newIcon {
		t.Error("a new yellow group (different fill level) emitted no NewIcon")
	}
	last := calls[len(calls)-1]
	if rev, ok := last.args[0].(uint32); !ok || rev != tr.revision || last.args[1] != trayIDRoot {
		t.Errorf("LayoutUpdated args = %v, want (%d, %d)", last.args, tr.revision, trayIDRoot)
	}
	if _, root, _ := tr.GetLayout(trayIDRoot, -1, nil); propOf(t, nodeChildren(root)[1], "label") != "Yellow — 2-9" {
		t.Errorf("layout still has the old label")
	}
}

func TestTrayToggleOnlyDiff(t *testing.T) {
	a := trayRowsFor(sliceSnapshot{Mode: "green", Cores: map[string]string{"green": "0-3"}})
	b := trayRowsFor(sliceSnapshot{Mode: "yellow", Cores: map[string]string{"green": "0-3"}})
	if _, ok := trayToggleOnlyDiff(a, a); ok {
		t.Error("identical rows reported as a toggle diff")
	}
	if upd, ok := trayToggleOnlyDiff(a, b); !ok || len(upd) != 2 {
		t.Errorf("tick move: ok=%v upd=%v, want two rows", ok, upd)
	}
	c := trayRowsFor(sliceSnapshot{Mode: "yellow", Cores: map[string]string{"green": "0-7"}})
	if _, ok := trayToggleOnlyDiff(a, c); ok {
		t.Error("a relabel reported as a toggle-only diff")
	}
	if _, ok := trayToggleOnlyDiff(a, b[:2]); ok {
		t.Error("different lengths reported as a toggle-only diff")
	}
}

// TestRunTrayCancelled: an already-cancelled context returns at once, without
// connecting to a bus.
func TestRunTrayCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { RunTray(ctx, newFakeTrayBackend("green")); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunTray did not return for a cancelled context")
	}
}
