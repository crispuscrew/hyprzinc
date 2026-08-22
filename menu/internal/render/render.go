// Package render draws the picker into an in-memory RGBA image with an antialiased monospace
// font (pure Go, no cgo; a system Nerd Font when one is installed, else the bundled Go Mono -
// see font.go). Frame is a pure function of the model, the palette, the per-frame view state,
// and the pixel size, so the whole look is unit-testable without a display: the menu's Wayland
// layer only has to blit the pixels Frame returns into a shared-memory buffer. Software
// rendering is ample for a small, redraw-on-keystroke menu window.
//
// Frame returns PREMULTIPLIED-alpha pixels (what Wayland surfaces expect), so translucency,
// the rounded corners, and the fade-in composite correctly over the desktop.
package render

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"

	"github.com/crispuscrew/zinc/menu/internal/picker"
	"github.com/crispuscrew/zinc/menu/internal/theme"
)

const (
	marginX      = 18 // left/right inset
	marginY      = 16 // top inset for the prompt
	headerH      = 46 // prompt line plus the separator beneath it
	rowH         = 22
	footerH      = 30
	bannerH      = 24 // launch-error / busy banner, shown above the footer when present
	barW         = 3  // accent bar down the left of the selected/error row
	dotSize      = 6  // running-app indicator
	nameX        = marginX + dotSize + 8
	cornerRadius = 12
	descGap      = 2  // spaces between the name column and the description
	descColMax   = 18 // cap on the name-column width used to line descriptions up
	iconGap      = 8  // space between the icon column and the name
)

// IconSize is the pixel size icons are scaled to and drawn at; the caller pre-scales to this.
const IconSize = 16

// Grid-layout geometry (used only when View.Grid is set).
const (
	gridGap      = 14   // gap between grid cells
	cellPad      = 6    // inner padding around a cell's thumbnail
	cellLabelH   = rowH // label row height beneath a cell's thumbnail
	defaultCellW = 180  // cell width when View.CellW is unset
	defaultCellH = 140  // cell height when View.CellH is unset
)

// View is the transient, per-frame state the renderer needs beyond the model: the entrance
// fade (0..1), the steady-state background opacity (0..1), an optional launch error or busy
// status to surface in a banner, and the layout (list by default, or a thumbnail grid).
type View struct {
	Prompt  string
	Footer  string // the hint line at the bottom (default "up/down move   enter select   esc quit")
	Fade    float64
	Opacity float64
	Error   string
	// Status is an in-progress activation ("launching nvim..."), drawn in the same banner slot
	// as Error but in the accent colour. Error wins if somehow both are set; a caller shows one
	// or the other, never both, since an activation that is still running has not failed yet.
	Status string

	// Grid switches from the default one-per-row list to a thumbnail grid, drawing each item's
	// Preview image (fetched through Thumb) as a tile with its label beneath.
	Grid  bool
	CellW int // grid cell width in px (default defaultCellW)
	CellH int // grid cell height in px (default defaultCellH)
	// Thumb returns the decoded, box-fitted thumbnail for a preview path, or nil if it is not
	// ready yet (drawn as a placeholder tile). It is called only for the cells actually on
	// screen, so a caller can lazily decode exactly what is visible. Ignored unless Grid.
	Thumb func(preview string) *image.RGBA
}

// Frame renders the picker at width x height pixels into a fresh RGBA image using pal and
// view. It never panics on an empty model or a tiny size (it clamps to at least one row).
func Frame(mdl *picker.Model, pal theme.Palette, view View, width, height int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	fill(img, img.Bounds(), pal.BG)

	drawPromptHeader(img, pal, view, mdl, width)

	visible := mdl.Visible()
	listTop := headerH + 4
	listBottom := height - footerH
	if view.Error != "" || view.Status != "" {
		listBottom -= bannerH
	}
	if listBottom < listTop+rowH {
		listBottom = listTop + rowH
	}

	switch {
	case len(visible) == 0:
		drawText(img, marginX, listTop+faceAscent, pal.Dim, "no matches")
	case view.Grid:
		drawGrid(img, mdl, pal, view, visible, width, listTop, listBottom)
	default:
		drawList(img, mdl, pal, visible, width, listTop, listBottom)
	}

	switch {
	case view.Error != "":
		drawBanner(img, pal, width, height, view.Error, pal.Error)
	case view.Status != "":
		drawBanner(img, pal, width, height, view.Status, pal.Accent)
	}
	drawFooter(img, pal, width, height, view.Footer, len(visible))

	finish(img, view.Opacity, view.Fade)
	return img
}

// drawPromptHeader draws the prompt line (accent prompt, query, block cursor) and the divider
// beneath it - shared by the list and grid layouts.
func drawPromptHeader(img *image.RGBA, pal theme.Palette, view View, mdl *picker.Model, width int) {
	prompt := view.Prompt
	if prompt == "" {
		prompt = "> "
	}
	promptBaseline := marginY + faceAscent
	drawText(img, marginX, promptBaseline, pal.Accent, prompt)
	queryX := marginX + runeLen(prompt)*faceAdvance
	drawText(img, queryX, promptBaseline, pal.FG, mdl.Query())
	cursorX := queryX + runeLen(mdl.Query())*faceAdvance
	fill(img, image.Rect(cursorX, marginY, cursorX+faceAdvance, marginY+faceHeight), pal.Accent)
	// Separator under the prompt (a subtle, theme-matched divider).
	fill(img, image.Rect(marginX, headerH-1, width-marginX, headerH), pal.SelBG)
}

// drawList renders the default one-item-per-row list into [listTop,listBottom), grouping into
// sections only when idle (typing flattens to the ranked list). visible is non-empty.
func drawList(img *image.RGBA, mdl *picker.Model, pal theme.Palette, visible []picker.App, width, listTop, listBottom int) {
	rows := (listBottom - listTop) / rowH
	if rows < 1 {
		rows = 1
	}
	grouping := mdl.Query() == "" && anyGroup(visible)
	display := buildRows(visible, grouping)
	descCol := descColumn(visible)
	iconCol := anyIcon(visible)
	start := scrollStart(rowForItem(display, mdl.Cursor()), len(display), rows)
	for offset := 0; offset < rows && start+offset < len(display); offset++ {
		row := display[start+offset]
		y := listTop + offset*rowH
		if row.isHeader {
			drawHeader(img, pal, width, y, row.header)
		} else {
			drawRow(img, pal, width, y, visible[row.item], descCol, iconCol, row.item == mdl.Cursor())
		}
	}
}

// drawGrid renders the items as a thumbnail grid into [listTop,listBottom): as many CellW-wide
// columns as fit, scrolling by whole rows to keep the cursor visible. visible is non-empty.
func drawGrid(img *image.RGBA, mdl *picker.Model, pal theme.Palette, view View, visible []picker.App, width, listTop, listBottom int) {
	cellW, cellH := view.CellW, view.CellH
	if cellW <= 0 {
		cellW = defaultCellW
	}
	if cellH <= 0 {
		cellH = defaultCellH
	}
	cols := GridColumns(width, cellW)
	rows := (listBottom - listTop + gridGap) / (cellH + gridGap)
	if rows < 1 {
		rows = 1
	}
	used := cols*cellW + (cols-1)*gridGap
	originX := marginX + (width-2*marginX-used)/2
	if originX < marginX {
		originX = marginX
	}
	start := gridScrollStart(mdl.Cursor(), len(visible), cols, rows)
	for index := 0; index < cols*rows && start+index < len(visible); index++ {
		column := index % cols
		row := index / cols
		x := originX + column*(cellW+gridGap)
		y := listTop + row*(cellH+gridGap)
		drawCell(img, pal, view, x, y, cellW, cellH, visible[start+index], start+index == mdl.Cursor())
	}
}

// drawCell draws one grid cell at pixel (x,y): a selection band and accent border when
// selected, the thumbnail (or a placeholder tile until it decodes) centered in the thumbnail
// box, and the label centered beneath it.
func drawCell(img *image.RGBA, pal theme.Palette, view View, x, y, cellW, cellH int, app picker.App, selected bool) {
	boxLeft := x + cellPad
	boxTop := y + cellPad
	boxW := cellW - 2*cellPad
	boxH := cellH - cellLabelH - cellPad
	if boxW < 1 {
		boxW = 1
	}
	if boxH < 1 {
		boxH = 1
	}

	if selected {
		fill(img, image.Rect(x, y, x+cellW, y+cellH), pal.SelBG)
		strokeRect(img, image.Rect(x, y, x+cellW, y+cellH), 2, pal.Accent)
	}

	// A backing tile so an undecoded cell still reads as a tile, and any letterbox margins
	// around the fitted thumbnail frame it rather than showing the raw background.
	fill(img, image.Rect(boxLeft, boxTop, boxLeft+boxW, boxTop+boxH), tileColor(pal, selected))
	var thumb *image.RGBA
	if view.Thumb != nil && app.Preview != "" {
		thumb = view.Thumb(app.Preview)
	}
	if thumb != nil {
		thumbW := thumb.Bounds().Dx()
		thumbH := thumb.Bounds().Dy()
		if thumbW > boxW {
			thumbW = boxW
		}
		if thumbH > boxH {
			thumbH = boxH
		}
		tx := boxLeft + (boxW-thumbW)/2
		ty := boxTop + (boxH-thumbH)/2
		draw.Draw(img, image.Rect(tx, ty, tx+thumbW, ty+thumbH), thumb, image.Point{}, draw.Over)
	}

	// Label beneath the thumbnail, centered and truncated to the cell width.
	labelColor := pal.FG
	if selected {
		labelColor = pal.SelFG
	}
	maxChars := boxW / faceAdvance
	text := truncate(app.Name, maxChars)
	labelX := x + (cellW-runeLen(text)*faceAdvance)/2
	if labelX < x+cellPad {
		labelX = x + cellPad
	}
	labelBaseline := y + cellH - cellLabelH + (cellLabelH-faceHeight)/2 + faceAscent
	drawText(img, labelX, labelBaseline, labelColor, text)
}

// tileColor is the colour behind a cell's thumbnail: a panel one step off the layer it sits
// on, so the tile stays visible whether or not its thumbnail has decoded.
func tileColor(pal theme.Palette, selected bool) color.Color {
	if selected {
		return pal.BG
	}
	return pal.SelBG
}

// strokeRect draws a hollow rectangle (four filled edges of the given thickness) in col.
func strokeRect(img *image.RGBA, rect image.Rectangle, thickness int, col color.Color) {
	fill(img, image.Rect(rect.Min.X, rect.Min.Y, rect.Max.X, rect.Min.Y+thickness), col)
	fill(img, image.Rect(rect.Min.X, rect.Max.Y-thickness, rect.Max.X, rect.Max.Y), col)
	fill(img, image.Rect(rect.Min.X, rect.Min.Y, rect.Min.X+thickness, rect.Max.Y), col)
	fill(img, image.Rect(rect.Max.X-thickness, rect.Min.Y, rect.Max.X, rect.Max.Y), col)
}

// GridColumns returns how many CellW-wide cells fit across an overlay of the given pixel
// width, at least one. The menu uses it to translate an Up/Down keypress into a whole-row
// cursor jump, matching what drawGrid lays out.
func GridColumns(width, cellW int) int {
	if cellW <= 0 {
		cellW = defaultCellW
	}
	avail := width - 2*marginX
	cols := (avail + gridGap) / (cellW + gridGap)
	if cols < 1 {
		cols = 1
	}
	return cols
}

// ThumbBox returns the pixel box a thumbnail is fit into inside a cellW x cellH grid cell, so
// a caller's thumbnail store scales to exactly the size drawCell draws.
func ThumbBox(cellW, cellH int) (int, int) {
	if cellW <= 0 {
		cellW = defaultCellW
	}
	if cellH <= 0 {
		cellH = defaultCellH
	}
	boxW := cellW - 2*cellPad
	boxH := cellH - cellLabelH - cellPad
	if boxW < 1 {
		boxW = 1
	}
	if boxH < 1 {
		boxH = 1
	}
	return boxW, boxH
}

// gridScrollStart returns the index of the first item to draw so the cursor's row stays on
// screen, scrolling by whole rows once the cursor would fall past the last visible row.
func gridScrollStart(cursor, count, cols, rows int) int {
	if cols < 1 {
		cols = 1
	}
	cursorRow := cursor / cols
	totalRows := (count + cols - 1) / cols
	startRow := 0
	if cursorRow >= rows {
		startRow = cursorRow - rows + 1
	}
	if startRow+rows > totalRows {
		startRow = totalRows - rows
	}
	if startRow < 0 {
		startRow = 0
	}
	return startRow * cols
}

// descColumn returns the column (in characters) at which descriptions line up: the longest
// visible name, capped, so short names share a tidy column and long names just push past it.
func descColumn(visible []picker.App) int {
	longest := 0
	for _, app := range visible {
		if nameLen := runeLen(app.Name); nameLen > longest {
			longest = nameLen
		}
	}
	if longest > descColMax {
		longest = descColMax
	}
	return longest
}

// drawRow renders one app row at pixel row-top y: a highlight band and accent bar when
// selected, an icon (when the column is active) or a running dot, the name, and a dim
// description aligned to descCol. iconCol keeps the name column aligned across rows even for
// rows whose own icon is missing.
func drawRow(img *image.RGBA, pal theme.Palette, width, top int, app picker.App, descCol int, iconCol, selected bool) {
	nameColor := pal.FG
	if selected {
		fill(img, image.Rect(0, top, width, top+rowH), pal.SelBG)
		fill(img, image.Rect(0, top, barW, top+rowH), pal.Accent)
		nameColor = pal.SelFG
	}
	textX := nameX
	if iconCol {
		textX = marginX + IconSize + iconGap
		switch {
		case app.Icon != nil:
			iconTop := top + (rowH-IconSize)/2
			draw.Draw(img, image.Rect(marginX, iconTop, marginX+IconSize, iconTop+IconSize), app.Icon, image.Point{}, draw.Over)
			if app.Running {
				dotX := marginX + IconSize - dotSize // a small badge on the icon's bottom-right corner
				dotY := top + (rowH+IconSize)/2 - dotSize
				fill(img, image.Rect(dotX, dotY, dotX+dotSize, dotY+dotSize), pal.Running)
			}
		case app.Running:
			drawDot(img, pal, marginX, top) // no icon in this row: center the dot in the cell
		}
	} else if app.Running {
		drawDot(img, pal, marginX, top)
	}
	baseline := top + (rowH-faceHeight)/2 + faceAscent
	drawText(img, textX, baseline, nameColor, app.Name)
	if app.Description != "" {
		column := descCol
		if nameLen := runeLen(app.Name); nameLen > column {
			column = nameLen // a name past the column pushes its own description along
		}
		descX := textX + (column+descGap)*faceAdvance
		maxChars := (width - descX - marginX) / faceAdvance
		drawText(img, descX, baseline, pal.Dim, truncate(app.Description, maxChars))
	}
}

// drawDot draws the running-indicator dot centered in the leftmost column at left,top.
func drawDot(img *image.RGBA, pal theme.Palette, left, top int) {
	dotTop := top + (rowH-dotSize)/2
	fill(img, image.Rect(left, dotTop, left+dotSize, dotTop+dotSize), pal.Running)
}

// anyIcon reports whether any visible item has a resolved icon (so the icon column shows).
func anyIcon(visible []picker.App) bool {
	for _, app := range visible {
		if app.Icon != nil {
			return true
		}
	}
	return false
}

// displayRow is one row in the rendered list: either a section header or an item (an index
// into the visible slice).
type displayRow struct {
	header   string
	item     int
	isHeader bool
}

// buildRows expands the visible items into rendered rows, inserting a section header before
// each new group when grouping is on. Items sharing a group should be adjacent in visible (the
// caller orders them) so a group's header renders once.
func buildRows(visible []picker.App, grouping bool) []displayRow {
	if !grouping {
		rows := make([]displayRow, len(visible))
		for index := range visible {
			rows[index] = displayRow{item: index}
		}
		return rows
	}
	var rows []displayRow
	current := "\x00" // a sentinel no real group equals, so the first item always opens a header
	for index, app := range visible {
		if app.Group != current {
			current = app.Group
			name := app.Group
			if name == "" {
				name = "Other"
			}
			rows = append(rows, displayRow{header: name, isHeader: true})
		}
		rows = append(rows, displayRow{item: index})
	}
	return rows
}

// rowForItem returns the display-row index of a visible item, so scrolling can keep the
// selected item on screen.
func rowForItem(rows []displayRow, item int) int {
	for index, row := range rows {
		if !row.isHeader && row.item == item {
			return index
		}
	}
	return 0
}

func anyGroup(visible []picker.App) bool {
	for _, app := range visible {
		if app.Group != "" {
			return true
		}
	}
	return false
}

// drawHeader draws a section-header row: the group name in the accent color, clipped to width.
func drawHeader(img *image.RGBA, pal theme.Palette, width, top int, name string) {
	baseline := top + (rowH-faceHeight)/2 + faceAscent
	maxChars := (width - marginX - marginX) / faceAdvance
	drawText(img, marginX, baseline, pal.Accent, truncate(name, maxChars))
}

// drawBanner draws a message just above the footer, with a coloured bar down its left edge: a
// failed launch in the error colour, an activation still running in the accent colour. Either
// way it is reported in the window rather than on a terminal nobody is looking at.
func drawBanner(img *image.RGBA, pal theme.Palette, width, height int, message string, col color.Color) {
	top := height - footerH - bannerH
	fill(img, image.Rect(0, top, width, top+bannerH), pal.SelBG)
	fill(img, image.Rect(0, top, barW, top+bannerH), col)
	baseline := top + (bannerH-faceHeight)/2 + faceAscent
	maxChars := (width - nameX - marginX) / faceAdvance
	drawText(img, nameX, baseline, col, truncate(message, maxChars))
}

// drawFooter draws a divider and the hint line, with the match count right-aligned.
func drawFooter(img *image.RGBA, pal theme.Palette, width, height int, hint string, count int) {
	if hint == "" {
		hint = "up/down move   enter select   esc quit"
	}
	top := height - footerH
	fill(img, image.Rect(marginX, top, width-marginX, top+1), pal.SelBG)
	baseline := top + (footerH-faceHeight)/2 + faceAscent
	drawText(img, marginX, baseline, pal.Dim, hint)
	countText := fmt.Sprintf("%d shown", count)
	countX := width - marginX - runeLen(countText)*faceAdvance
	drawText(img, countX, baseline, pal.Dim, countText)
}

// finish turns the opaque drawing into the final surface: it rounds the corners and applies
// the steady-state opacity and the entrance fade, writing premultiplied alpha so the result
// composites correctly over whatever is behind the overlay.
func finish(img *image.RGBA, opacity, fade float64) {
	global := clamp01(opacity) * clamp01(fade)
	width, height := img.Bounds().Dx(), img.Bounds().Dy()
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			alpha := global * cornerCoverage(x, y, width, height, cornerRadius)
			offset := img.PixOffset(x, y)
			img.Pix[offset+0] = scale(img.Pix[offset+0], alpha)
			img.Pix[offset+1] = scale(img.Pix[offset+1], alpha)
			img.Pix[offset+2] = scale(img.Pix[offset+2], alpha)
			img.Pix[offset+3] = scale(0xff, alpha)
		}
	}
}

// cornerCoverage returns how much of the pixel lies inside the rounded rectangle: 1 in the
// body, 0 outside a corner, and a fractional value on a corner's antialiased edge.
func cornerCoverage(x, y, width, height, radius int) float64 {
	if radius <= 0 {
		return 1
	}
	var centerX, centerY float64
	switch {
	case x < radius && y < radius:
		centerX, centerY = float64(radius), float64(radius)
	case x >= width-radius && y < radius:
		centerX, centerY = float64(width-radius), float64(radius)
	case x < radius && y >= height-radius:
		centerX, centerY = float64(radius), float64(height-radius)
	case x >= width-radius && y >= height-radius:
		centerX, centerY = float64(width-radius), float64(height-radius)
	default:
		return 1
	}
	deltaX := float64(x) + 0.5 - centerX
	deltaY := float64(y) + 0.5 - centerY
	coverage := float64(radius) - math.Sqrt(deltaX*deltaX+deltaY*deltaY) + 0.5
	return clamp01(coverage)
}

// scale multiplies an 8-bit channel by an alpha fraction (used to premultiply).
func scale(channel uint8, alpha float64) uint8 {
	return uint8(float64(channel)*alpha + 0.5)
}

func clamp01(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}

// scrollStart returns the first visible row so the cursor stays on screen (it scrolls only
// once the cursor would fall past the last visible row).
func scrollStart(cursor, total, rows int) int {
	if total <= rows {
		return 0
	}
	start := 0
	if cursor >= rows {
		start = cursor - rows + 1
	}
	if start+rows > total {
		start = total - rows
	}
	if start < 0 {
		start = 0
	}
	return start
}

func truncate(text string, maxChars int) string {
	runes := []rune(text)
	if maxChars < 0 {
		maxChars = 0
	}
	if len(runes) <= maxChars {
		return text
	}
	if maxChars <= 3 {
		return string(runes[:maxChars])
	}
	return string(runes[:maxChars-3]) + "..."
}

func drawText(img *image.RGBA, x, baseline int, col color.Color, text string) {
	drawer := font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(col),
		Face: face,
		Dot:  fixed.P(x, baseline),
	}
	drawer.DrawString(text)
}

func fill(img *image.RGBA, rect image.Rectangle, col color.Color) {
	draw.Draw(img, rect, image.NewUniform(col), image.Point{}, draw.Src)
}

func runeLen(text string) int { return len([]rune(text)) }
