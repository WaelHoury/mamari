(struct_item
  name: (type_identifier) @name) @definition.class

(enum_item
  name: (type_identifier) @name) @definition.class

(trait_item
  name: (type_identifier) @name) @definition.interface

(impl_item
  type: (type_identifier) @receiver.type
  body: (declaration_list
    (function_item
      name: (identifier) @name) @definition.method))

(impl_item
  trait: (_) @trait.name
  type: (_) @impl.type)

(function_item
  name: (identifier) @name) @definition.function
