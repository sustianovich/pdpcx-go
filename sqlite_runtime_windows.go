package main

// go-sqlite3 v0.35.6 traduce SQLite de WASM a Go mediante wasm2go.
// En Windows ya no hay RuntimeConfig de wazero ni intérprete que seleccionar.
// La memoria queda bajo la gestión del nuevo driver; no se trasladan los
// límites de páginas de memoria del intérprete anterior.
