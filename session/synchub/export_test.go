package synchub

// PadToBucket exposes the unexported padToBucket helper to the external test
// package so the padding ladder can be tested without changing its visibility.
var PadToBucket = padToBucket
