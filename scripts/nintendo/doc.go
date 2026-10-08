// Package nintendo provides a stateful Nintendo inventory monitoring script.
// It checks the Electric OCARINA OF TIME product page, then notifies when the
// item becomes purchasable. Reuse one Script instance across serial runs so it
// can distinguish a transition from an initial result.
package nintendo
