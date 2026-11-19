// Package retro is the house style of the project's full-screen views: a retro disk-tool look
// with a blue screen, white text, yellow titles and highlight, thin single-line boxes, a grey
// title bar on top and a grey message bar at the bottom. docs/reference/house-style.md describes it.
//
// The package draws on a tcell screen and builds on tview primitives. It knows nothing about
// installing or backing up; programs give it texts and states.
package retro

import (
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// The reference colours (the 16-colour text-mode palette of the era).
var (
	rgbBlue   = tcell.NewRGBColor(0x55, 0x55, 0xff) // screen
	rgbNavy   = tcell.NewRGBColor(0x00, 0x00, 0xaa) // input fields
	rgbWhite  = tcell.NewRGBColor(0xff, 0xff, 0xff) // text, frames, blocks with data
	rgbYellow = tcell.NewRGBColor(0xff, 0xff, 0x55) // titles, highlight, blocks done
	rgbGrey   = tcell.NewRGBColor(0xaa, 0xaa, 0xaa) // bars, examples
	rgbBlack  = tcell.NewRGBColor(0x00, 0x00, 0x00) // text on the bars
	rgbRed    = tcell.NewRGBColor(0xaa, 0x00, 0x00) // messages on the bottom bar, errors
)

// Glyphs are the characters of frames, blocks and bars: the CP437 look through Unicode, or
// plain ASCII when the terminal cannot show Unicode.
type Glyphs struct {
	H, V, TL, TR, BL, BR rune // frame
	Sep                  rune // separator on the bottom bar
	Shade                rune // empty block, empty progress track
	Block                rune // block with data
	Full                 rune // filled progress track; a done block without colour
	DoneShade            rune // a done empty block without colour
	Unread               rune // a block not read yet, nothing known about it
	Current, Done        rune // list marks for the current and the finished steps
}

var (
	unicodeGlyphs = Glyphs{H: '─', V: '│', TL: '┌', TR: '┐', BL: '└', BR: '┘', Sep: '│',
		Shade: '░', Block: '◘', Full: '█', DoneShade: '▒', Unread: '·', Current: '►', Done: '√'}
	asciiGlyphs = Glyphs{H: '-', V: '|', TL: '+', TR: '+', BL: '+', BR: '+', Sep: '|',
		Shade: '.', Block: 'o', Full: '#', DoneShade: ':', Unread: ' ', Current: '>', Done: '*'}
)

// Theme holds the styles and glyphs of one screen.
type Theme struct {
	Color bool // false: NO_COLOR or a terminal without colours; attributes only
	ASCII bool // the terminal's character set is not UTF-8
	G     Glyphs

	Base      tcell.Style // text on the screen
	Strong    tcell.Style // headers, the important word
	Title     tcell.Style // box titles
	Frame     tcell.Style // box frames
	Bar       tcell.Style // title bar and bottom bar
	BarKey    tcell.Style // a key label on the title bar (inverse)
	BarMsg    tcell.Style // the message on the bottom bar
	Focus     tcell.Style // the focused row or button
	Field     tcell.Style // a text field
	Example   tcell.Style // an example in an empty text field
	Error     tcell.Style // an error message
	CellEmpty tcell.Style // block map cells
	CellData  tcell.Style
	CellDone  tcell.Style
	CellWork  tcell.Style // the block being read or written
	Track     tcell.Style // progress track: empty part
	TrackFull tcell.Style // progress track: filled part
}

// NewTheme returns the theme for a screen: color false gives the same layout without
// colours (NO_COLOR), and a character set other than UTF-8 gives ASCII glyphs.
func NewTheme(screen tcell.Screen, color bool) Theme {
	return themeFor(screen.Colors(), screen.CharacterSet(), color)
}

// themeFor picks the palette for a colour depth: exact colours with 256 colours or more,
// the 16 standard colours, or the 8 basic colours with bold for the bright ones.
func themeFor(colors int, charset string, color bool) Theme {
	t := Theme{Color: color && colors >= 8}
	cs := strings.ToUpper(strings.ReplaceAll(charset, "-", ""))
	t.ASCII = cs != "UTF8"
	t.G = unicodeGlyphs
	if t.ASCII {
		t.G = asciiGlyphs
	}
	if !t.Color {
		def := tcell.StyleDefault
		t.Base, t.Strong, t.Title, t.Frame = def, def.Bold(true), def.Bold(true), def
		t.Bar, t.BarKey, t.BarMsg = def.Reverse(true), def, def.Reverse(true).Bold(true)
		t.Focus, t.Field, t.Example, t.Error = def.Reverse(true), def.Underline(true), def.Dim(true).Underline(true), def.Bold(true)
		t.CellEmpty, t.CellData, t.CellDone, t.CellWork = def, def, def.Bold(true), def.Reverse(true)
		t.Track, t.TrackFull = def, def
		return t
	}
	blue, navy, white, yellow, grey, black, red := rgbBlue, rgbNavy, rgbWhite, rgbYellow, rgbGrey, rgbBlack, rgbRed
	var bright tcell.AttrMask
	switch {
	case colors >= 256:
	case colors >= 16:
		blue, navy, white, yellow, grey, black, red = tcell.ColorBlue, tcell.ColorNavy, tcell.ColorWhite,
			tcell.ColorYellow, tcell.ColorSilver, tcell.ColorBlack, tcell.ColorMaroon
	default: // 8 colours: the screen is the dark blue, bold makes the text bright
		blue, navy, white, yellow, grey, black, red = tcell.ColorNavy, tcell.ColorBlack, tcell.ColorSilver,
			tcell.ColorOlive, tcell.ColorSilver, tcell.ColorBlack, tcell.ColorMaroon
		bright = tcell.AttrBold
	}
	on := tcell.StyleDefault.Background(blue)
	t.Base = on.Foreground(white).Attributes(bright)
	t.Strong = t.Base.Bold(true)
	t.Title = on.Foreground(yellow).Attributes(bright)
	t.Frame = t.Base
	t.Bar = tcell.StyleDefault.Background(grey).Foreground(black)
	t.BarKey = tcell.StyleDefault.Background(black).Foreground(white).Attributes(bright)
	t.BarMsg = tcell.StyleDefault.Background(grey).Foreground(red)
	t.Focus = tcell.StyleDefault.Background(yellow).Foreground(blue)
	t.Field = tcell.StyleDefault.Background(navy).Foreground(white).Attributes(bright)
	t.Example = tcell.StyleDefault.Background(navy).Foreground(grey)
	t.Error = tcell.StyleDefault.Background(red).Foreground(white).Bold(true)
	t.CellEmpty = t.Base
	t.CellData = t.Base
	t.CellDone = t.Title
	t.CellWork = t.Base.Bold(true)
	t.Track = t.Base
	t.TrackFull = t.Base
	return t
}

// Apply makes tview's own primitives (boxes, borders, titles) follow the theme. It changes
// tview's package settings, so a program calls it once before it draws.
func (t Theme) Apply() {
	fg, bg, _ := t.Base.Decompose()
	title, _, _ := t.Title.Decompose()
	tview.Styles.PrimitiveBackgroundColor = bg
	tview.Styles.ContrastBackgroundColor = bg
	tview.Styles.MoreContrastBackgroundColor = bg
	tview.Styles.BorderColor = fg
	tview.Styles.TitleColor = title
	tview.Styles.GraphicsColor = fg
	tview.Styles.PrimaryTextColor = fg
	tview.Styles.SecondaryTextColor = title
	tview.Styles.TertiaryTextColor = fg
	tview.Styles.InverseTextColor = bg
	tview.Styles.ContrastSecondaryTextColor = title
	g := t.G
	// a box looks the same with and without focus: thin single lines
	tview.Borders.Horizontal, tview.Borders.HorizontalFocus = g.H, g.H
	tview.Borders.Vertical, tview.Borders.VerticalFocus = g.V, g.V
	tview.Borders.TopLeft, tview.Borders.TopLeftFocus = g.TL, g.TL
	tview.Borders.TopRight, tview.Borders.TopRightFocus = g.TR, g.TR
	tview.Borders.BottomLeft, tview.Borders.BottomLeftFocus = g.BL, g.BL
	tview.Borders.BottomRight, tview.Borders.BottomRightFocus = g.BR, g.BR
}

// Ready wraps a screen that is already initialised, for tview.Application.SetScreen, which
// would initialise it again.
func Ready(screen tcell.Screen) tcell.Screen { return ready{screen} }

type ready struct{ tcell.Screen }

func (ready) Init() error { return nil }
