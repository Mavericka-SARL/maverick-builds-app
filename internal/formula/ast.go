package formula

type Node interface {
	nodeTag()
}

type NumberLit struct{ Val float64 }
type StringLit struct{ Val string }
type BoolLit struct{ Val bool }
type Ident struct{ Name string }

type UnaryExpr struct {
	Op   string
	Expr Node
}

type BinaryExpr struct {
	Op    string
	Left  Node
	Right Node
}

type CallExpr struct {
	Name string
	Args []Node
}

func (*NumberLit) nodeTag()  {}
func (*StringLit) nodeTag()  {}
func (*BoolLit) nodeTag()    {}
func (*Ident) nodeTag()      {}
func (*UnaryExpr) nodeTag()  {}
func (*BinaryExpr) nodeTag() {}
func (*CallExpr) nodeTag()   {}
