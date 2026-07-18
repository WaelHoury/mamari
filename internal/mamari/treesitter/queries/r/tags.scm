; R functions are bound by assignment: `name <- function(...)` or `name = function(...)`.
(binary_operator
  lhs: (identifier) @name
  rhs: (function_definition)) @definition.function
