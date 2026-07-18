(class_definition
  name: (identifier) @name) @definition.class

(mixin_declaration
  (identifier) @name) @definition.class

(enum_declaration
  name: (identifier) @name) @definition.class

(method_signature
  (function_signature
    name: (identifier) @name)) @definition.method

(method_signature
  (constructor_signature
    name: (identifier) @name)) @definition.method

(method_signature
  (factory_constructor_signature
    (identifier) @name)) @definition.method

(declaration
  (constructor_signature
    name: (identifier) @name)) @definition.method


(program
  (function_signature
    name: (identifier) @name) @definition.function)
