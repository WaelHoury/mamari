(struct_specifier
  name: (type_identifier) @name
  body: (field_declaration_list)) @definition.class

(union_specifier
  name: (type_identifier) @name
  body: (field_declaration_list)) @definition.class

(enum_specifier
  name: (type_identifier) @name
  body: (enumerator_list)) @definition.class

(function_definition
  declarator: (function_declarator
    declarator: (identifier) @name)) @definition.function

(function_definition
  declarator: (pointer_declarator
    declarator: (function_declarator
      declarator: (identifier) @name))) @definition.function
