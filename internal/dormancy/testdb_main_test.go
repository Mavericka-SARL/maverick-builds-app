package dormancy_test

import (
	"os"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }
