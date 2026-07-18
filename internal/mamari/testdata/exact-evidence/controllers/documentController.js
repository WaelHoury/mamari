async function downloadEnvelopeDocuments(req) {
  return Buffer.from('document')
}

export async function previewEnvelopeDocuments(req, res) {
  const body = await downloadEnvelopeDocuments(req)
  res.setHeader('Content-Type', 'application/pdf')
  return res.end(body)
}
