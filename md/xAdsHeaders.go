package md

// XAdsHeaders (x-ads-...) are used when specifying the translation jobs
type XAdsHeaders struct {
	// Format => x-ads-derivative-format header, "latest" (Default) or "fallback"
	// Specifies how to interpret the formats.advanced.objectIds request body parameter for OBJ output.
	// If you use this header with one derivative (URN), you must use it consistently across the following endpoints, whenever you reference the same derivative.
	// - POST job (for OBJ output)
	// - GET {urn}/metadata/{guid}
	// - GET {urn}/metadata/{guid}/properties
	Format DerivativeFormat
	// Overwrite => x-ads-force header: false (default) or true
	Overwrite bool
}

// NewXAdsHeaders gets XAdsHeaders with the given values.
//  - format  =>  x-ads-derivative-format header:
//    Possible values are: "latest" or "fallback"
//  - overwrite  =>  x-ads-force header;
//    Possible values are: false or true
func NewXAdsHeaders(format DerivativeFormat, overwrite bool) XAdsHeaders {
	xAdsHeaders := XAdsHeaders{}
	xAdsHeaders.Format = format
	xAdsHeaders.Overwrite = overwrite
	return xAdsHeaders
}

// DefaultXAdsHeaders gets XAdsHeaders with default values
func DefaultXAdsHeaders() XAdsHeaders {
	xHeaders := XAdsHeaders{}
	xHeaders.Format = Latest
	xHeaders.Overwrite = false
	return xHeaders
}

// DerivativeFormat Indicates the value for the xAdsHeaders.Format
type DerivativeFormat string

const (
	Latest   DerivativeFormat = "latest"   // (Default) Consider formats.advanced.objectIds to be SVF2 Object IDs.
	FallBack DerivativeFormat = "fallback" // Consider formats.advanced.objectIds to be SVF Object IDs.
)