package treesitter

// Def describes a class, function, or method definition.
type Def struct {
	Kind          string // "class" | "function" | "method" | "callback"
	Name          string
	QualifiedName string

	NameStart, NameEnd int
	Start, End         int // includes decorator lines for decorated defs

	ParentName, ParentKind string // "" if module-level
	ParentStart            int    // byte offset, disambiguates same-named parents

	Exported bool

	// Bases lists base-class expressions for "class" defs, e.g. ["Base"] or
	// ["pkg.Base", "Mixin"]. Empty for non-class defs or classes with no
	// explicit base list.
	Bases []string

	// Interfaces lists implemented-interface simple names for "class" defs
	// (Java `implements` clause), e.g. ["Repository", "Serializable"]. Empty
	// for non-class defs or classes with no `implements` clause.
	Interfaces []string

	// Annotations lists Java annotations attached to this def (class or
	// method), e.g. `@RequestMapping("/api/owners")` ->
	// {Name: "RequestMapping", Value: "/api/owners"}. Empty for non-Java defs
	// or defs with no annotations.
	Annotations []Annotation

	// ReceiverName and ReceiverType describe a Go method's receiver, e.g.
	// `func (s *Service) Foo()` -> ReceiverName "s", ReceiverType "Service"
	// (pointer stripped). Both are "" for non-Go defs, non-method defs, or
	// methods with an unnamed receiver (`func (Service) Foo()`).
	ReceiverName string
	ReceiverType string

	// ReturnTypes lists the simple (pointer-stripped) return type names of a
	// Go "function"/"method" def, in declaration order, e.g.
	// `func NewThing() *Thing` -> ["Thing"], `func NewPair() (int, error)`
	// -> ["int", "error"]. An element is "" if that return value's type
	// couldn't be simplified to a name (e.g. an inline struct/func type).
	// Empty for non-Go defs or defs with no return value.
	ReturnTypes []string

	// IsPartial reports whether a C# "class" def carries the `partial`
	// modifier (`public partial class Service { ... }`) — a class split
	// across multiple files (designer-generated code, source generators,
	// EF scaffolding), where each file's `partial class Service` is a
	// separate fragment of one logical class, not a separate class. False
	// for every other language and for non-partial C# classes.
	IsPartial bool
}

// Annotation describes a single Java annotation attached to a class or
// method declaration, e.g. `@GetMapping("/{id}")` or
// `@PostMapping(value = "/new", produces = "application/json")`. Value holds
// the first `value`/`path` string argument (or the sole positional string
// argument), or "" if the annotation has no such argument.
type Annotation struct {
	Name  string
	Value string

	// Method holds the HTTP method named by a Spring `@RequestMapping(method
	// = RequestMethod.POST)` element (or the first element of `method =
	// {RequestMethod.GET, RequestMethod.POST}`), e.g. "POST". "" if absent.
	Method string
}

// VarDecl describes a field, local variable, or formal parameter declaration
// with its simple (unqualified, non-generic) type name, used to resolve
// `variable.method()` calls to the variable's declared type.
type VarDecl struct {
	Name    string
	Type    string
	AliasOf string // source binding for assignments such as `self.repo = repo`
	Pos     int    // byte offset of the enclosing declaration, for scope lookup
}

// Call describes a function or method call expression.
type Call struct {
	Callee, Receiver string // Receiver set only for attribute calls (e.g. "self", "cls", "obj")
	Start, End       int
	// Constructor is true for a syntactically unambiguous construction
	// expression synthesized from a declaration rather than an ordinary
	// function call. Resolvers must match it to a class-like symbol only.
	Constructor bool

	// InDecorator is true when the call is the expression (or part of the
	// expression) of a decorator, e.g. `@app.route("/x")`. Decorators run at
	// module-definition time, not as part of the decorated function's body.
	InDecorator bool
}

// CallAssign describes a Go short variable declaration whose right-hand side
// is a single call expression, e.g. `x := NewThing()` -> {Name: "x",
// Receiver: "", Callee: "NewThing", ResultIndex: 0} or
// `x, err := s.GetRepo()` -> {Name: "x", Receiver: "s", Callee: "GetRepo",
// ResultIndex: 0} (plus a second CallAssign for "err" at ResultIndex 1).
// Used to infer x's declared type from the callee's return-type signature
// (Def.ReturnTypes) for `x.Method()` call resolution.
type CallAssign struct {
	Name        string
	Receiver    string // "" for bare calls, else the receiver expression text
	Callee      string
	ResultIndex int
	Pos         int // byte offset of the short_var_declaration, for scope lookup
}

// Import describes a single imported name binding.
type Import struct {
	Spec       string // normalized module spec, incl. relative ("." / ".models")
	Name       string // imported member for Python `from module import Name`
	Alias      string // "" unless `as` used
	Start, End int
}

// HCLBlock describes one native-syntax HCL block. TopLevel reports whether
// the block is declared directly in the file body (as Terraform declarations
// are), rather than nested inside another block. Labels contain decoded static
// string labels in source order.
type HCLBlock struct {
	Type               string
	Labels             []string
	Start, End         int
	HeaderEnd          int
	TopLevel           bool
	ParentBlockStart   int
	TopLevelBlockStart int
}

// HCLAttribute describes an HCL body attribute and its owning blocks.
// StaticValue is populated only for a plain quoted string with no template
// interpolation, which is enough for Terraform provider aliases, module
// sources, and declaration descriptions.
type HCLAttribute struct {
	Name               string
	StaticValue        string
	Start, End         int
	NameStart, NameEnd int
	BlockStart         int
	TopLevelBlockStart int
}

// HCLTraversal is a dotted named-value traversal found in an expression,
// such as var.region, data.aws_ami.ubuntu.id, or aws_instance.web[0].id.
// Parts excludes index expressions and splats; consumers select the address
// prefix relevant to the Terraform declaration kind.
type HCLTraversal struct {
	Parts              []string
	Start, End         int
	AttributeStart     int
	AttributeName      string
	TopLevelBlockStart int
}

// ParseResult is the structural result of parsing a source file.
type ParseResult struct {
	Defs        []Def
	Calls       []Call
	Imports     []Import
	Vars        []VarDecl
	CallAssigns []CallAssign
	HCLBlocks   []HCLBlock
	HCLAttrs    []HCLAttribute
	HCLRefs     []HCLTraversal

	// Package is the Java `package a.b.c;` declaration, e.g. "a.b.c", or ""
	// for non-Java files or files with no package declaration (default
	// package).
	Package string

	// ParseOK/ParseError surface tree-sitter's root-node error-recovery
	// state. Callers map these onto File.ParseStatus/ParseError.
	ParseOK    bool
	ParseError string
}
