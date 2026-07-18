(call
  target: (identifier) @elixir.keyword
  (arguments . (alias) @name)) @definition.class

(call
  target: (identifier) @elixir.keyword
  (arguments . (call
    target: (identifier) @name))) @definition.function

(call
  target: (identifier) @elixir.keyword
  (arguments . (binary_operator
    left: (call
      target: (identifier) @name)))) @definition.function
