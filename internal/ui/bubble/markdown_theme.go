// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"charm.land/glamour/v2/ansi"

	"github.com/TaraTheStar/enso/internal/ui/theme"
)

// buildMarkdownTheme returns a glamour StyleConfig built from the
// shared enso palette. Built dynamically so that ~/.enso/theme.toml
// overrides flow through to assistant-message markdown the same way
// they flow through every other styled element.
//
// Design intent: recede, don't shout. Glamour's default themes
// (dark, dracula, etc.) wrap headings in coloured backgrounds and
// give inline code a heavy block; that loudness clashes with enso's
// otherwise muted palette and makes assistant replies visually
// dominate the rest of the chat. This theme uses foreground colour
// only — no backgrounds — and biases toward the same lavender /
// mauve / comment values the rest of the UI already uses.
func buildMarkdownTheme(pal theme.Palette) ansi.StyleConfig {
	hex := func(name string) *string {
		if c, ok := pal[name]; ok {
			s := c.Hex()
			return &s
		}
		return nil
	}

	indentBar := "▎ "
	zero := uint(0)
	one := uint(1)
	two := uint(2)
	tru := true

	return ansi.StyleConfig{
		// Margin 0 keeps glamour content left-flush so the "enso › "
		// prefix on line 1 reads naturally and continuation lines
		// align at column 0 — same shape as raw text would have.
		Document: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				BlockPrefix: "",
				BlockSuffix: "",
			},
			Margin: &zero,
		},
		// Block quotes pick up the same dim left bar as the reasoning
		// block so quoted content reads as "set apart but not loud."
		BlockQuote: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: hex("comment")},
			Indent:         &one,
			IndentToken:    &indentBar,
		},
		Paragraph: ansi.StyleBlock{},
		List: ansi.StyleList{
			StyleBlock:  ansi.StyleBlock{},
			LevelIndent: 2,
		},

		// Headings stay in the assistant's accent (lavender) — they
		// echo the role label rather than introducing a new colour.
		// All sizes use the same colour; the hash prefix length carries
		// the level hierarchy on its own.
		Heading: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				BlockSuffix: "\n",
				Color:       hex("lavender"),
				Bold:        &tru,
			},
		},
		H1: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "# ",
				Color:  hex("lavender"),
				Bold:   &tru,
			},
		},
		H2: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Prefix: "## "}},
		H3: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Prefix: "### "}},
		H4: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Prefix: "#### "}},
		H5: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Prefix: "##### "}},
		H6: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Prefix: "###### ",
				Color:  hex("comment"),
			},
		},

		// Inline emphasis uses ANSI attributes only — no colour change
		// — so prose reads in the terminal's foreground without the
		// rainbow-text effect glamour's defaults produce.
		Text:          ansi.StylePrimitive{},
		Strikethrough: ansi.StylePrimitive{CrossedOut: &tru},
		Emph:          ansi.StylePrimitive{Italic: &tru},
		Strong:        ansi.StylePrimitive{Bold: &tru},

		HorizontalRule: ansi.StylePrimitive{
			Color:  hex("comment"),
			Format: "\n────\n",
		},

		Item:        ansi.StylePrimitive{BlockPrefix: "• "},
		Enumeration: ansi.StylePrimitive{BlockPrefix: ". "},
		Task: ansi.StyleTask{
			StylePrimitive: ansi.StylePrimitive{},
			Ticked:         "[✓] ",
			Unticked:       "[ ] ",
		},

		Link: ansi.StylePrimitive{
			Color:     hex("teal"),
			Underline: &tru,
		},
		LinkText: ansi.StylePrimitive{
			Color: hex("teal"),
			Bold:  &tru,
		},

		Image: ansi.StylePrimitive{
			Color:     hex("lavender"),
			Underline: &tru,
		},
		ImageText: ansi.StylePrimitive{
			Color:  hex("lavender"),
			Format: "Image: {{.text}} →",
		},

		// Inline `code` spans: mauve foreground, no background. The
		// background block in glamour's defaults reads as a heavy chip
		// against terminal default; foreground-only is enough.
		Code: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Color: hex("mauve"),
			},
		},

		// Fenced ```lang code blocks: chroma-syntax-highlighted using
		// enso's palette. Margin 2 indents code visibly without the
		// aggressive ▎ bar — left to chroma colours to mark the region.
		CodeBlock: ansi.StyleCodeBlock{
			StyleBlock: ansi.StyleBlock{
				StylePrimitive: ansi.StylePrimitive{Color: hex("gray")},
				Margin:         &two,
			},
			Chroma: chromaTheme(pal),
		},

		Table:                 ansi.StyleTable{},
		DefinitionList:        ansi.StyleBlock{},
		DefinitionTerm:        ansi.StylePrimitive{},
		DefinitionDescription: ansi.StylePrimitive{BlockPrefix: "\n→ "},
		HTMLBlock:             ansi.StyleBlock{},
		HTMLSpan:              ansi.StyleBlock{},
	}
}

// chromaTheme returns the syntax-highlighting palette for fenced code
// blocks. Token classes are mapped to enso's palette rather than
// chroma's defaults so highlighted code visually belongs to the same
// chat surface as the surrounding prose. No backgrounds anywhere —
// terminal background shows through, which avoids the "code-block
// chip" look that conflicts with our otherwise minimal styling.
func chromaTheme(pal theme.Palette) *ansi.Chroma {
	hex := func(name string) *string {
		if c, ok := pal[name]; ok {
			s := c.Hex()
			return &s
		}
		return nil
	}
	tru := true

	return &ansi.Chroma{
		Text:                ansi.StylePrimitive{},
		Error:               ansi.StylePrimitive{Color: hex("red")},
		Comment:             ansi.StylePrimitive{Color: hex("comment"), Italic: &tru},
		CommentPreproc:      ansi.StylePrimitive{Color: hex("comment")},
		Keyword:             ansi.StylePrimitive{Color: hex("lavender")},
		KeywordReserved:     ansi.StylePrimitive{Color: hex("mauve")},
		KeywordNamespace:    ansi.StylePrimitive{Color: hex("mauve")},
		KeywordType:         ansi.StylePrimitive{Color: hex("teal")},
		Operator:            ansi.StylePrimitive{Color: hex("gray")},
		Punctuation:         ansi.StylePrimitive{Color: hex("gray")},
		Name:                ansi.StylePrimitive{},
		NameBuiltin:         ansi.StylePrimitive{Color: hex("mauve")},
		NameTag:             ansi.StylePrimitive{Color: hex("mauve")},
		NameAttribute:       ansi.StylePrimitive{Color: hex("dust")},
		NameClass:           ansi.StylePrimitive{Color: hex("lavender"), Bold: &tru},
		NameConstant:        ansi.StylePrimitive{Color: hex("dust")},
		NameDecorator:       ansi.StylePrimitive{Color: hex("dust")},
		NameException:       ansi.StylePrimitive{Color: hex("red")},
		NameFunction:        ansi.StylePrimitive{Color: hex("lavender"), Bold: &tru},
		NameOther:           ansi.StylePrimitive{},
		Literal:             ansi.StylePrimitive{},
		LiteralNumber:       ansi.StylePrimitive{Color: hex("dust")},
		LiteralDate:         ansi.StylePrimitive{Color: hex("dust")},
		LiteralString:       ansi.StylePrimitive{Color: hex("sage")},
		LiteralStringEscape: ansi.StylePrimitive{Color: hex("dust")},
		// Generic inserted/deleted match the diff coloring on tool
		// output so a code block containing a diff snippet uses the
		// same green/red as the structural diff render.
		GenericDeleted:    ansi.StylePrimitive{Color: hex("red")},
		GenericEmph:       ansi.StylePrimitive{Italic: &tru},
		GenericInserted:   ansi.StylePrimitive{Color: hex("sage")},
		GenericStrong:     ansi.StylePrimitive{Bold: &tru},
		GenericSubheading: ansi.StylePrimitive{Color: hex("comment")},
		// No Background entry — leave terminal background alone.
	}
}
