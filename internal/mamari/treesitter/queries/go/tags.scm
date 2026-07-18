(function_declaration
  name: (identifier) @name) @definition.function

(method_declaration
  name: (field_identifier) @name) @definition.method

(type_spec
  name: (type_identifier) @name
  type: (struct_type)) @definition.class

(type_spec
  name: (type_identifier) @name
  type: (interface_type)) @definition.interface
