; long-form:  function f(x) ... end
(function_definition
  (signature
    (call_expression
      .
      (identifier) @name))) @definition.function

; short-form:  f(x) = expr   (assignment whose lhs is a call_expression)
(assignment
  (call_expression
    .
    (identifier) @name)) @definition.function

(struct_definition
  (type_head
    (identifier) @name)) @definition.class

; struct T <: Super  — name is the first identifier of the subtype expression
(struct_definition
  (type_head
    (binary_expression
      .
      (identifier) @name))) @definition.class

(abstract_definition
  (type_head
    (identifier) @name)) @definition.class

(module_definition
  name: (identifier) @name) @definition.class
