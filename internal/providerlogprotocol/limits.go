// Package providerlogprotocol defines the bounded wire contract shared by the
// provider-host log bridge and its AGS client.
package providerlogprotocol

const (
	// DecodedTextMaxBytes is the maximum decoded provider log carried by one response.
	DecodedTextMaxBytes int64 = 4 << 20

	// JSON can expand each decoded byte to a six-byte \uXXXX escape. The fixed
	// margin bounds schema and binding metadata around the text field.
	EncodedEnvelopeMarginBytes int64 = 64 << 10
	EncodedResponseMaxBytes          = DecodedTextMaxBytes*6 + EncodedEnvelopeMarginBytes
)
