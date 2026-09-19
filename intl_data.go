package quickjs

// Carry the complete localized display-name table by default. The package's
// init only registers the compressed bytes; individual locales remain lazily
// decoded when Intl.DisplayNames first asks for them.
import _ "github.com/go-quickjs/go-quickjs/intldata"
