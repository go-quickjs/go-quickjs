//go:build !quickjs_verify

package mir

// verifyBudgets is set by the quickjs_verify build tag
// (budgetverify_on.go).
const verifyBudgets = false

func (c *core) checkBudgets() {}
