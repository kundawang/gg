# GSUB contextual shaping fixtures

`gsub_context_latin.ttf` and `gsub_context_arabic.ttf` exercise the GSUB
contextual engine (lookup types 5, 6 and 8, all supported subtable formats).
They are derived from `goregular.ttf` with extra glyphs appended via
fontTools, so every substituted glyph ID is drawable and stable across
regenerations.

## gsub_context_latin.ttf

One lookup per OpenType construct, each reachable through its own feature:

| Feature | Lookup | Construct |
|---------|--------|-----------|
| `calt`  | 5      | Context format 1 (glyph rules), nested single lookups in record order |
| `clig`  | 6      | Context format 2 (class rules) |
| `salt`  | 7      | Context format 3 (coverage sequence) |
| `dlig`  | 8      | Chain context format 1, backtrack + lookahead |
| `cswh`  | 9      | Chain context format 2 (class based) |
| `ccmp`  | 10     | Chain context format 3 (coverage based) |
| `rvrn`  | 11     | Reverse chain single format 1 |
| `rclt`  | 12     | Reverse chain "format 3" — fontTools serializes an empty subtable, used to verify safe stopping |
| `med2`  | 13     | IgnoreMarks context driving an IgnoreMarks ligature |
| `rlig`  | 14     | Self-referencing context (lookup cycle guard) |
| `init`/`medi`/`mset`/`mk2f`/`mk3f` | 15–21 | GDEF mark filtering sets, direct and nested |

The font also contains a plain `liga` ligature (`fi`) and a GDEF 1.2 table
with a GlyphClassDef (marks are class 3) and one MarkGlyphSet covering only
the `g_mk2` mark.

## gsub_context_arabic.ttf

RTL chain context lookups (`cf01`/`cf02`/`cf03`, formats 1/2/3) producing
isolated/initial presentation forms for four Arabic letters. All nested
lookups carry the RightToLeft flag; the engine keeps the buffer in logical
order and matches backtrack/lookahead against fixed physical directions.

## Reference sequences

Expected glyph sequences were captured from HarfBuzz 15.0.0 with every other
feature explicitly disabled, so each golden case isolates one lookup. The
Go golden tests live in `gsub_context_fixture_test.go` (table-driven, glyph
IDs hard-coded against this fixture) and `gsub_e2e_test.go` (full
`OwnShaper.Shape` pipeline including cluster propagation).

Regeneration requires `fontTools` and `uharfbuzz`; the generator and golden
capture scripts are kept out of the module and are not needed to run tests.
