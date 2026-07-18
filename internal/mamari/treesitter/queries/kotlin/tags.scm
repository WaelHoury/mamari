(class_declaration
  "class"
  name: (identifier) @name) @definition.class

(class_declaration
  "interface"
  name: (identifier) @name) @definition.interface

(function_declaration
  name: (identifier) @name) @definition.function
