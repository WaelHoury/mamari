(call_expression) @call

; typed parameters and `var/const name: Type = ...` declarations feed
; receiver-type resolution for `recv.method()` calls.
(parameter) @vardecl

(variable_declaration) @vardecl
