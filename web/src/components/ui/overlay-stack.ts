import { shallowReactive } from 'vue'

export const overlayStack = shallowReactive<symbol[]>([])
let previousOverflow = ''
let background: Element | null = null
let previousInert: string | null = null

export function acquireOverlay(id: symbol) {
  if (!overlayStack.length) {
    previousOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    background = document.querySelector('#app')
    previousInert = background?.getAttribute('inert') ?? null
    background?.setAttribute('inert', '')
  }
  overlayStack.push(id)
}

export function releaseOverlay(id: symbol) {
  const index = overlayStack.indexOf(id)
  if (index < 0) return
  overlayStack.splice(index, 1)
  if (!overlayStack.length) {
    document.body.style.overflow = previousOverflow
    if (previousInert === null) background?.removeAttribute('inert')
    else background?.setAttribute('inert', previousInert)
    background = null
  }
}
