// Package archtest holds the tests that keep the package layering in place.
//
// The rules are checked against the import statements of the non-test sources,
// so a change that reintroduces a forbidden dependency fails here with the
// rule it broke, instead of surfacing later as a channel that has to be added
// in a dozen places.
package archtest
