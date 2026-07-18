; a let-binding with at least one parameter is a function.
(value_definition
  (let_binding
    pattern: (value_name) @name
    (parameter))) @definition.function

(module_definition
  (module_binding
    (module_name) @name)) @definition.class

(module_type_definition
  (module_type_name) @name) @definition.interface

(class_definition
  (class_binding
    (class_name) @name)) @definition.class

(method_definition
  (method_name) @name) @definition.method

(type_definition
  (type_binding
    name: (type_constructor) @name)) @definition.class
