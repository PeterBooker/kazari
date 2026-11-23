package site

const MaxCompressSize = maxCompressSize

var (
	NoneMatch = noneMatch
	Match     = match
)

// AcceptWeight is the weight Accept-Encoding field values give a coding.
func AcceptWeight(values []string, name string) float64 {
	return parseAcceptEncoding(values).of(name)
}

// CodingETag is the ETag of a file's representation in the coding name.
func CodingETag(hash, name string) string {
	return `"` + hash + "-" + name + `"`
}
