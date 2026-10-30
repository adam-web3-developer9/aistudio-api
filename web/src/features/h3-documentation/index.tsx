import { Link } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'

import {
  CodeBlock,
  CodeBlockCopyButton,
} from '@/components/ai-elements/code-block'
import { PublicLayout } from '@/components/layout'

import {
  h3PollExample,
  h3ReferenceExample,
  h3ResponseExample,
  h3SubmitExample,
} from './examples'

export function H3GuideContent() {
  const { t } = useTranslation()
  return (
    <article className='mx-auto max-w-4xl space-y-10 pb-12 leading-relaxed'>
      <header className='space-y-4'>
        <p className='text-primary text-sm font-medium'>
          New API → Studio → AIGate ComfyUI
        </p>
        <h1 className='text-3xl font-semibold tracking-tight'>
          {t('H3 API documentation')}
        </h1>
        <p className='text-muted-foreground'>
          {t(
            'Generate H3 videos through the AIGate compute pool. Your application uses only the New API endpoint and its own API key.'
          )}
        </p>
        <p>
          <Link
            to='/pricing'
            className='text-primary underline underline-offset-4'
          >
            {t('Model Market')}
          </Link>
        </p>
      </header>

      <section className='space-y-3' aria-labelledby='h3-prepare'>
        <h2 id='h3-prepare' className='text-xl font-semibold'>
          {t('1. Prepare your API key and wallet')}
        </h2>
        <p>
          {t(
            'Sign in, open Console → API keys, and copy an enabled API key. Its group must have access to h3, its model restrictions must permit h3, and both wallet and key credit must cover the video. Dashboard access tokens and Studio operator keys are not generation API keys.'
          )}
        </p>
        <p>
          <Link
            to='/keys'
            className='text-primary underline underline-offset-4'
          >
            {t('API keys')}
          </Link>
        </p>
        <p>
          {t(
            'H3 uses wallet credit at a fixed $0.09 per requested video second: 4 seconds costs $0.36, 5 seconds $0.45, and 15 seconds $1.35. Group discounts do not apply. These are USD platform-credit units; the recharge page shows the RMB amount payable.'
          )}
        </p>
        <p>
          {t(
            'Credit is deducted when the task is durably accepted. Confirmed failure refunds it once. Queue, startup, upload, rendering and archiving time are not extra billable video seconds. A network timeout alone does not establish failure or trigger a refund.'
          )}
        </p>
      </section>

      <section className='space-y-3' aria-labelledby='h3-submit'>
        <h2 id='h3-submit' className='text-xl font-semibold'>
          {t('2. Submit a text-to-video task')}
        </h2>
        <p>
          {t(
            'The examples use Bash-compatible shell syntax. Replace the API key and choose a unique request ID before running. This POST starts a paid generation; the 5-second example costs $0.45 platform credit.'
          )}
        </p>
        <CodeBlock code={h3SubmitExample} language='bash' showToolbar>
          <CodeBlockCopyButton />
        </CodeBlock>
        <p>
          {t(
            'Use model h3 and a nonempty prompt. seconds is an integer from 4 to 15 (default 5); an integer string is also accepted. size defaults to 1344x768. Width and height must each be multiples of 32 from 480 to 1920, with at most 2088960 pixels. 1280x720 is invalid because 720 is not a multiple of 32.'
          )}
        </p>
        <p>
          {t(
            'An accepted request returns HTTP 202 with a public task id. The following is an illustrative response; stage and progress depend on the current task. Keep the returned id for subsequent queries.'
          )}
        </p>
        <CodeBlock code={h3ResponseExample} language='json' showToolbar>
          <CodeBlockCopyButton />
        </CodeBlock>
      </section>

      <section className='space-y-3' aria-labelledby='h3-poll'>
        <h2 id='h3-poll' className='text-xl font-semibold'>
          {t('3. Poll and download the completed video')}
        </h2>
        <p>
          {t(
            'Query the same id approximately every 5 seconds. status is queued, in_progress, completed or failed. metadata.stage explains stages such as booting, awaiting_upload and archiving. Completed means the archived video has been verified. Download only after completed; save the file before expires_at (Unix seconds), normally within 24 hours of archival.'
          )}
        </p>
        <CodeBlock code={h3PollExample} language='bash' showToolbar>
          <CodeBlockCopyButton />
        </CodeBlock>
        <p>
          {t(
            'Use the same account for retrieval and keep h3 permitted on the API key. The content endpoint requires Authorization. Do not interpret a temporary empty result, queue wait or archiving stage as a failed generation.'
          )}
        </p>
      </section>

      <section className='space-y-3' aria-labelledby='h3-media'>
        <h2 id='h3-media' className='text-xl font-semibold'>
          {t('4. Add image or audio references')}
        </h2>
        <p>
          {t(
            'Send descriptors in metadata.direct_media, not files, image URLs, base64 or input_reference. Image bindings are image1–image9; audio bindings are audio1–audio3. Use safe ASCII filenames with letters, digits, underscores or hyphens (maximum 200 characters), the exact MIME type and lowercase SHA-256 of the original file.'
          )}
        </p>
        <p>
          {t(
            'Supported files: png/image/png, jpg or jpeg/image/jpeg, webp/image/webp, wav/audio/wav, mp3/audio/mpeg. The filename extension, MIME type and image/audio binding must agree. Declare only files actually needed by the prompt.'
          )}
        </p>
        <p>
          {t(
            'When metadata.stage is awaiting_upload, metadata.upload supplies baseUrl, subfolder, files and reservationId. Upload each file directly to the current GPU target, then send only its receipt to POST /v1/videos/{id}/inputs with your New API key. Never send your New API key or a Studio operator key to the GPU upload URL.'
          )}
        </p>
        <p>
          {t(
            'The following Python example uses one image and one audio file and performs submission, direct upload, receipt acknowledgement, polling and download. Remove unused media entries to use fewer references. Without references, the service automatically advances the empty upload step.'
          )}
        </p>
        <CodeBlock
          code={h3ReferenceExample}
          language='python'
          showToolbar
          enableCollapse
          defaultCollapsed
          collapsedLines={18}
        >
          <CodeBlockCopyButton />
        </CodeBlock>
        <p>
          {t(
            'A reservation belongs to a particular GPU allocation. On COMFY_UPLOAD_RESERVATION_STALE (409), fetch the same video again and upload to its new target using its new reservationId. Do not reuse an old target or create a new generation.'
          )}
        </p>
      </section>

      <section className='space-y-3' aria-labelledby='h3-retry'>
        <h2 id='h3-retry' className='text-xl font-semibold'>
          {t('5. Retry and handle errors safely')}
        </h2>
        <p>
          {t(
            'Keep the same Idempotency-Key, API key and exact JSON request when retrying a submission after a timeout or lost response. A new intended video needs a new key. Reusing a key with different input returns 409. Keys must be 1–128 characters without surrounding whitespace; their scope includes the account and API key.'
          )}
        </p>
        <dl className='space-y-3 text-sm'>
          <div>
            <dt className='font-mono font-semibold'>400</dt>
            <dd>
              {t(
                'Invalid JSON, prompt, seconds, size or media descriptor. Correct the request before starting a new intended generation.'
              )}
            </dd>
          </div>
          <div>
            <dt className='font-mono font-semibold'>401 / 403</dt>
            <dd>
              {t(
                'Check the generation API key, key status, model permissions and account access. Do not use the dashboard token.'
              )}
            </dd>
          </div>
          <div>
            <dt className='font-mono font-semibold'>402</dt>
            <dd>
              {t(
                'Insufficient wallet or API-key credit. Top up or adjust the key limit before retrying.'
              )}
            </dd>
          </div>
          <div>
            <dt className='font-mono font-semibold'>409</dt>
            <dd>
              {t(
                'Submission: the idempotency key conflicts with different input. Input receipt: refresh the current upload reservation and resend matching receipts.'
              )}
            </dd>
          </div>
          <div>
            <dt className='font-mono font-semibold'>502 / 503 / timeout</dt>
            <dd>
              {t(
                'Keep the original task and request key. Query an existing task, or replay the same submission if its response was lost. Do not create duplicate videos to work around an uncertain network result.'
              )}
            </dd>
          </div>
          <div>
            <dt className='font-mono font-semibold'>status = failed</dt>
            <dd>
              {t(
                'Read error.code and retain the task id for support. A confirmed generation failure is refunded once; an expired or temporarily unavailable video that was already delivered is not a new refundable generation.'
              )}
            </dd>
          </div>
        </dl>
      </section>
    </article>
  )
}

export function H3Documentation() {
  return (
    <PublicLayout>
      <H3GuideContent />
    </PublicLayout>
  )
}
