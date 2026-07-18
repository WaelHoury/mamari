(command) @call

(command
  name: (command_name (word) @method)
  argument: (word) @import_path
  (#any-of? @method "source" ".")) @import_bash
