; class_declaration covers class/struct/actor AND `extension Type { ... }`
; (Swift's grammar gives extensions the exact same node shape as a real
; class/struct declaration, just with declaration_kind="extension" — so
; methods added in an extension nest under the extended type's name here
; with no special-case overlay needed, the same outcome Rust's impl-block
; overlay and Kotlin's extension-receiver-detection helper exist to
; produce by other means).
(class_declaration
  name: (type_identifier) @name) @definition.class

; `extension Type { ... }` shares class_declaration's shape (see the
; comment above), but its name field is a user_type node wrapping a nested
; type_identifier, not a bare type_identifier directly — found by dumping
; the actual parse tree after this case alone silently failed to capture
; (extension methods fell back to being parented under whatever definition
; preceded the extension lexically, e.g. an unrelated class earlier in the
; same file, rather than under the extended type).
(class_declaration
  name: (user_type (type_identifier) @name)) @definition.class

(protocol_declaration
  name: (type_identifier) @name) @definition.interface

(class_declaration
    (class_body
        [
            (function_declaration
                name: (simple_identifier) @name
            )
            (subscript_declaration
                (parameter (simple_identifier) @name)
            )
            (init_declaration "init" @name)
            (deinit_declaration "deinit" @name)
        ]
    )
) @definition.method

(protocol_declaration
    (protocol_body
        [
            (protocol_function_declaration
                name: (simple_identifier) @name
            )
            (subscript_declaration
                (parameter (simple_identifier) @name)
            )
            (init_declaration "init" @name)
        ]
    )
) @definition.method

(function_declaration
    name: (simple_identifier) @name) @definition.function
