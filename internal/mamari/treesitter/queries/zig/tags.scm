(function_declaration
  name: (identifier) @name) @definition.function

; container types are bound by `const Name = struct/enum/union { ... }`.
(variable_declaration
  .
  (identifier) @name
  (struct_declaration)) @definition.class

(variable_declaration
  .
  (identifier) @name
  (enum_declaration)) @definition.class

(variable_declaration
  .
  (identifier) @name
  (union_declaration)) @definition.class
