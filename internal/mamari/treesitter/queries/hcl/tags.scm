; Generic HCL blocks become symbols named by their last label. Native
; Terraform/OpenTofu .tf files use the Terraform-aware emitter instead:
; canonical addresses and declaration-specific kinds cannot be represented
; by this generic query alone.
;
; single-label blocks (module "x", variable "x", output "x", provider "x").
(block
  .
  (identifier)
  .
  (string_lit (template_literal) @name)
  .
  (block_start)) @definition.class

; two-label blocks (resource "type" "name", data "type" "name") — the second
; label is the block's own name.
(block
  .
  (identifier)
  .
  (string_lit)
  .
  (string_lit (template_literal) @name)
  .
  (block_start)) @definition.class
