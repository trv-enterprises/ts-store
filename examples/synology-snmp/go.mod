// Separate module: this collector needs gosnmp for SNMPv3/USM (key
// derivation, engine discovery, AES-CFB128 privacy). Keeping it out of the
// root go.mod preserves ts-store's zero-dependency posture for the server
// and the stdlib-only collectors.
module github.com/tviviano/ts-store/examples/synology-snmp

go 1.25.5

require github.com/gosnmp/gosnmp v1.44.0
