(class_definition
  name: (identifier) @name) @definition.class

(function_definition
  name: (identifier) @name) @definition.function

(assignment
  left: (identifier) @name
  right: (lambda)) @definition.callback
