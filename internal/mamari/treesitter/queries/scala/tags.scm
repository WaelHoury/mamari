(class_definition
  name: (identifier) @name
  body: (template_body)) @definition.class

(object_definition
  name: (identifier) @name
  body: (template_body)) @definition.class

(trait_definition
  name: (identifier) @name
  body: (template_body)) @definition.interface

(function_definition
  name: (identifier) @name) @definition.function
