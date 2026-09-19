# ICU word-break dictionaries

These files are unmodified copies of the word-break sources from ICU 78.3:

https://github.com/unicode-org/icu/tree/release-78.3/icu4c/source/data/brkitr/dictionaries

Their copyright and license notices are retained at the start of each file.
`cldrgen` removes comments, sorts the entries, and independently compresses
each dictionary into `tables.bin`, so a runtime only decodes scripts it uses.
