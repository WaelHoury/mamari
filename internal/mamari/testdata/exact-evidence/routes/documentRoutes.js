import { previewEnvelopeDocuments } from '../controllers/documentController'

router.post('/signing/:id/preview', previewEnvelopeDocuments)
