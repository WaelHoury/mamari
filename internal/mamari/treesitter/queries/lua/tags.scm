(function_declaration
  name: (identifier) @name) @definition.function

(function_declaration
  name: (dot_index_expression
    table: (identifier) @receiver.type
    field: (identifier) @name)) @definition.method

(function_declaration
  name: (method_index_expression
    table: (identifier) @receiver.type
    method: (identifier) @name)) @definition.method
