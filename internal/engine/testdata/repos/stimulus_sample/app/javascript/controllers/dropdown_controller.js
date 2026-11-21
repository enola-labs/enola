import { Controller } from "@hotwired/stimulus"

export default class DropdownController extends Controller {
  toggle() {
    this.element.classList.toggle("open")
  }
}
