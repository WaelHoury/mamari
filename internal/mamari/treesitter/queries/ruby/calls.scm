(call) @call

(super) @call

(call
  method: (identifier) @method
  arguments: (argument_list (string))
  (#any-of? @method "require" "require_relative")) @import_ruby

(assignment) @vardecl
