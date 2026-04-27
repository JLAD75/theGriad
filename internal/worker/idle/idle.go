// Package idle reports how long the user has been inactive on the local
// machine. It backs the worker's BOINC-style "only contribute when the
// user is away" feature.
//
// Implementations are platform-specific. The package exposes a single
// SinceLastInput() function that returns a duration since the last
// keyboard or mouse input. On platforms without a real implementation,
// the stub returns a long duration so the worker defaults to "available".
package idle
