// Home page entry: a trivial counter to prove the hashed bundle is wired up.
const button = document.querySelector<HTMLButtonElement>('#counter')
const output = document.querySelector<HTMLElement>('#count')

let count = 0
button?.addEventListener('click', () => {
  count += 1
  if (output) output.textContent = String(count)
})

export {}
