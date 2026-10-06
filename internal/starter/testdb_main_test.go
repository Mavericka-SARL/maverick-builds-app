package starter

import (
	"os"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
)

// The shared test database's container goes when this package's tests are
// done, not whenever Ryuk next sweeps (see testdb.Run).
func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }
