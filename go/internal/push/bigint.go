package push

import "math/big"

// bigInt aliases big.Int so tests can construct one without importing math/big
// into every file that only needs it for signature verification.
type bigInt = big.Int
